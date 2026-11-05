Dreadnought private server launcher for Linux

Dreadnought is a Windows game, so on Linux it runs through Proton (Steam) or
Wine. This starts the normal launcher inside the same Wine/Proton setup the game
uses; the launcher opens in your web browser (sign-in, game folder, options, Play).

1. Unpack this folder anywhere.
2. Run:  ./dn-launcher-linux.sh
   Optional: ./dn-launcher-linux.sh --install   (adds a menu entry with icon)

It finds Dreadnought in your Steam libraries. If it is installed somewhere
else, tell it where (the folder that contains DreadGame/):
   DN_GAME_DIR="/path/to/Dreadnought" ./dn-launcher-linux.sh

Proton is used when the game already has a Proton prefix (you started
Dreadnought from Steam at least once); otherwise Wine, with its own prefix in
~/.local/share/dreadnought-wine. Force one with DN_RUNNER=proton or
DN_RUNNER=wine. Settings can also go in ~/.config/dreadnought-launcher/config.

Sign out / switch account:  ./dn-launcher-linux.sh --sign-out
