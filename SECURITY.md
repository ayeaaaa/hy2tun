# Security Policy

## Supported versions

Security fixes are provided for the latest release on the `main` branch.
Older releases may receive fixes at the maintainers' discretion.

## Reporting a vulnerability

Please do not open a public issue for a security vulnerability.

Use GitHub's private vulnerability reporting:

1. Open the repository's **Security** tab.
2. Choose **Report a vulnerability**.
3. Include the affected version, configuration, reproduction steps, and impact.

If private reporting is unavailable, contact the maintainer through the
repository owner profile and ask for a private channel. Do not include
passwords, control tokens, private keys, or production configuration files in
the first message.

## Scope

This project is a reverse tunnel and a Hysteria 2 proxy server/client. Relevant
security areas include:

- HY2 authentication and control-token separation
- reverse mapping token handling and port-policy enforcement
- web administrator authentication, CSRF, session handling, and rate limiting
- credential rotation, previous-credential retention, and revoke behavior
- configuration and state file permissions and atomic writes
- secret handling in logs, APIs, errors, and the web console
- denial of service through mappings, UDP sessions, or web endpoints

## Deployment assumptions

- The server web console is not exposed directly over plaintext HTTP on the
  public internet.
- The server and client run under a trusted administrator account.
- `web.trust_proxy` is enabled only behind a proxy that overwrites both
  `X-Forwarded-For` and `X-Real-IP`.
- The server's TLS certificate and private key are protected with restrictive
  file permissions.
- The control token and HY2 credentials are independent secrets.

## Known accepted vulnerability

`GO-2026-5288` affects Hysteria core v2.13.0: a specially constructed QUIC
packet can cause OOM when Hysteria's `sniff` feature is enabled. hy2tun does
not expose or enable `sniff`, and upstream has no fixed release yet. The
vulnerability is documented here and allowlisted in
`scripts/ci/govulncheck.sh`; it should be removed from the allowlist as soon
as a fixed Hysteria core is available.

## Out of scope

- Vulnerabilities in Hysteria 2 core or other upstream dependencies should be
  reported upstream as well; we will still track and update pinned versions.
- Misconfiguration that exposes the web console, state file, private keys, or
  configuration files to untrusted users.
- Attacks requiring root or physical access to the server or client host.
- Traffic-analysis or censorship-resistance guarantees beyond the documented
  obfuscation features.
