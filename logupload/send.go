package logupload

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"
)

// ErrNotSupported: the server has no chunked upload (a server older than the
// launcher). Send the log with Legacy instead.
var ErrNotSupported = errors.New("the server has no chunked log upload")

// RefusedError: the server will never take this log (HTTP 400, 413: bad data,
// too large), so trying again later is pointless.
type RefusedError struct{ Status int }

func (e *RefusedError) Error() string { return fmt.Sprintf("refused (HTTP %d)", e.Status) }

// Uploader sends a Manifest to the server.
type Uploader struct {
	Client *http.Client
	// Base is the server's log URL; parts go to Base+"/part", the commit to
	// Base+"/commit".
	Base  string
	Token string
	// Attempts per request (default 4) and the wait before the second one,
	// doubled each time (default 2 s).
	Attempts int
	Backoff  time.Duration
	// Progress, when set, is told after each part: parts sent, parts in all.
	Progress func(sent, total int)
	// PartQuery is added to every part request (the crash upload names its
	// report on each part: mmogbrain refuses a part without it).
	PartQuery url.Values
}

// NewUploadID is a fresh 128-bit upload id (32 hex digits).
func NewUploadID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on the platforms the launcher runs on;
		// an id only has to be unlikely to collide.
		binaryFallback(b[:])
	}
	return hex.EncodeToString(b[:])
}

func binaryFallback(b []byte) {
	n := time.Now().UnixNano()
	for i := range b {
		b[i] = byte(n >> (uint(i%8) * 8))
		n = n*6364136223846793005 + 1442695040888963407
	}
}

// reply is the server's JSON answer.
type reply struct {
	OK        bool   `json:"ok"`
	ID        int64  `json:"id"`
	Duplicate bool   `json:"duplicate"`
	Missing   []int  `json:"missing"`
	Error     string `json:"error"`
}

// Upload sends every part, then commits with the session's details in q
// (modified, exit, crashed, seconds, late, version). A server that already has
// this log (a retry) answers "duplicate" at any point, which ends the upload
// successfully. A part or the commit that fails is retried with a growing wait;
// a commit that finds parts missing gets just those again.
func (u *Uploader) Upload(m *Manifest, id string, q url.Values) error {
	for i := range m.Parts {
		done, err := u.sendPart(m, id, i, q.Get("modified"))
		if err != nil || done {
			return err
		}
		if u.Progress != nil {
			u.Progress(i+1, len(m.Parts))
		}
	}
	commit := url.Values{}
	for k, v := range q {
		commit[k] = v
	}
	commit.Set("id", id)
	commit.Set("parts", strconv.Itoa(len(m.Parts)))
	for round := 0; round < 3; round++ {
		var r reply
		status, err := u.post(u.Base+"/commit?"+commit.Encode(), nil, &r, true)
		if err != nil {
			return err
		}
		if status != http.StatusConflict {
			return nil
		}
		// Some parts are gone from the server (lost, or swept away): send them again.
		if len(r.Missing) == 0 {
			return errors.New("the server wants parts it did not name")
		}
		for _, i := range r.Missing {
			if i < 0 || i >= len(m.Parts) {
				return errors.New("the server named a part that does not exist")
			}
			if done, err := u.sendPart(m, id, i, q.Get("modified")); err != nil || done {
				return err
			}
		}
	}
	return errors.New("the server keeps missing parts")
}

// sendPart uploads part i; done is true when the server says it has the whole
// log already.
func (u *Uploader) sendPart(m *Manifest, id string, i int, modified string) (done bool, err error) {
	body, err := os.ReadFile(m.Parts[i].Path)
	if err != nil {
		return false, err
	}
	v := url.Values{}
	for k, vals := range u.PartQuery {
		v[k] = vals
	}
	v.Set("id", id)
	v.Set("n", strconv.Itoa(i))
	if modified != "" {
		v.Set("modified", modified)
	}
	var r reply
	if _, err := u.post(u.Base+"/part?"+v.Encode(), body, &r, false); err != nil {
		return false, err
	}
	return r.Duplicate, nil
}

// post sends one request, retrying a dropped connection and a 5xx/429 answer.
// It returns the final status; a 409 is returned (not an error) when
// allow409, 404/405 become ErrNotSupported and 400/413 a RefusedError.
func (u *Uploader) post(target string, body []byte, out *reply, allow409 bool) (int, error) {
	attempts, wait := u.Attempts, u.Backoff
	if attempts <= 0 {
		attempts = 4
	}
	if wait <= 0 {
		wait = 2 * time.Second
	}
	var last error
	for a := 0; a < attempts; a++ {
		if a > 0 {
			time.Sleep(wait)
			wait *= 2
		}
		req, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(body))
		if err != nil {
			return 0, err
		}
		req.Header.Set("Content-Type", "application/gzip")
		req.Header.Set("Authorization", "Bearer "+u.Token)
		resp, err := u.Client.Do(req)
		if err != nil {
			last = err
			continue
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
		*out = reply{}
		_ = json.Unmarshal(data, out)
		switch {
		case resp.StatusCode == http.StatusOK:
			return resp.StatusCode, nil
		case resp.StatusCode == http.StatusConflict && allow409:
			return resp.StatusCode, nil
		case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed:
			return resp.StatusCode, ErrNotSupported
		case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusRequestEntityTooLarge:
			return resp.StatusCode, &RefusedError{Status: resp.StatusCode}
		default:
			// 401 (sign-in expired), 429 (day limit, too soon), 5xx: not
			// worth hammering; a short retry covers a hiccup.
			last = fmt.Errorf("HTTP %d", resp.StatusCode)
			if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusTooManyRequests {
				return resp.StatusCode, last
			}
		}
	}
	return 0, last
}

// Legacy is the body of the one-POST upload older servers take (mmogbrain
// before the chunked endpoint: at most 48 MiB of text, 8 MiB compressed): the
// first part, the cut marker, and as many of the last parts as fit in
// maxRaw. The parts are gzip members already, so the body is just them one
// after the other, which the server's gzip reader reads as one stream.
func Legacy(m *Manifest, maxRaw int64) ([]byte, error) {
	if len(m.Parts) == 0 {
		return nil, io.ErrUnexpectedEOF
	}
	// Take parts from the end while they fit; the first part is always in.
	used := m.Parts[0].Raw + 128 // 128: the marker
	from := len(m.Parts)
	for i := len(m.Parts) - 1; i >= 1; i-- {
		if used+m.Parts[i].Raw > maxRaw {
			break
		}
		used += m.Parts[i].Raw
		from = i
	}
	var skipped int64
	for i := 1; i < from; i++ {
		skipped += m.Parts[i].Raw
	}
	var body bytes.Buffer
	add := func(i int) error {
		b, err := os.ReadFile(m.Parts[i].Path)
		if err != nil {
			return err
		}
		body.Write(b)
		return nil
	}
	if err := add(0); err != nil {
		return nil, err
	}
	if from > 1 {
		var mk bytes.Buffer
		zw := gzip.NewWriter(&mk)
		_, _ = zw.Write([]byte(CutMarker(skipped + m.Cut)))
		_ = zw.Close()
		body.Write(mk.Bytes())
	}
	for i := from; i < len(m.Parts); i++ {
		if err := add(i); err != nil {
			return nil, err
		}
	}
	return body.Bytes(), nil
}

// SendLegacy posts a Legacy body to the one-POST endpoint url.
func (u *Uploader) SendLegacy(target string, body []byte) error {
	var r reply
	_, err := u.post(target, body, &r, false)
	return err
}
