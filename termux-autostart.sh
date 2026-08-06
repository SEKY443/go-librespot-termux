#!/data/data/com.termux/files/usr/bin/sh
# Termux auto-start for running go-librespot as a home audio server.
#
# Sets up everything Termux itself needs to run go-librespot inside a
# proot Ubuntu: a wake lock (so Android doesn't suspend PulseAudio/sshd once
# Termux isn't the foreground app), PulseAudio listening on the TCP loopback
# (the proot can't reach Termux's own Unix sockets, only 127.0.0.1), and
# sshd for remote access - then drops you into the Ubuntu proot, where you
# run go-librespot itself as usual.
#
# Wire this into ~/.bashrc so it runs on every new Termux session:
#   echo 'source ~/termux-autostart.sh' >> ~/.bashrc

# 1. Acquire a wake lock so Android doesn't aggressively suspend Termux's
#    background processes once the app isn't in the foreground - without
#    this, PulseAudio (and the go-librespot session depending on it) can get
#    killed at unpredictable times.
if command -v termux-wake-lock >/dev/null 2>&1; then
    termux-wake-lock
fi

# 2. Start PulseAudio listening on the TCP loopback. A stale runtime
#    directory/socket left over from a previous session (e.g. after Android
#    killed PulseAudio) can prevent a clean restart, so it's cleared first.
if ! pgrep -x "pulseaudio" >/dev/null; then
    rm -rf "$HOME"/.config/pulse/* "$TMPDIR"/pulse-*
    pulseaudio --start \
        --load="module-native-protocol-tcp auth-anonymous=1 listen=127.0.0.1" \
        --exit-idle-time=-1
    echo "[+] PulseAudio initialized on 127.0.0.1:4713"
fi

# 3. Start sshd for remote access, so you don't need to be at the phone to
#    get a shell back into this session.
if ! pgrep -x "sshd" >/dev/null; then
    sshd
    echo "[+] SSH server initialized on port 8022"
fi

# 4. Drop into the Ubuntu proot automatically. PROOT_ACTIVE guards against
#    re-entering if this file gets sourced again from inside the proot
#    itself (e.g. a nested login shell).
if [ -z "$PROOT_ACTIVE" ]; then
    export PROOT_ACTIVE=1
    proot-distro login ubuntu
fi
