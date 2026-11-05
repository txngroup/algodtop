# Security

algod.top reads algod's API tokens, serves a web UI and an API, and `algod.top remote install` puts binaries on other hosts. A bug in any of these can expose a node.

Please report a vulnerability privately through GitHub: open the repository's Security tab and choose "Report a vulnerability". Do not open a public issue for it. Include the output of `algod.top version`, the platform, and how the daemon is set up.

Areas of most interest:

- A way to read algod tokens, the config or the store without the web password, or to write with a read-only token.
- A way past the web UI's Host check, CSRF token or Content-Security-Policy.
- A release whose binaries do not match its signed `checksums.txt`, or a way to make `remote install` accept a binary that is not signed by the release key.
- A log line, a peer address or an algod response that makes algod.top run a command or write outside its own directories.
