# Security policy

## Reporting a vulnerability

Please don't report security problems in public issues or pull requests.

Report them privately in either of these ways:

- Open a private report from this repository's **Security** tab: [Report a vulnerability](https://github.com/flaggr-dev/flaggr-go/security/advisories/new).
- Email [security@flaggr.dev](mailto:security@flaggr.dev).

Include the affected version, the steps to reproduce the problem and what an attacker could do with it. Don't include a real API token.

This covers the Go SDK in this repository. You can also report problems in the Flaggr service itself, such as `https://flaggr.dev` or `https://api.flaggr.dev`, to security@flaggr.dev.

## Supported versions

Fixes go into the latest release. Update with:

```bash
go get github.com/flaggr-dev/flaggr-go@latest
```
