# CLAUDE.md

@AGENTS.md

AGENTS.md is the single rule source. This file only adds what is specific to Claude Code:

- Write code; the owner runs `kgr` and the image build on the real host. Don't run them, or any
  `sudo`, `virsh`, `docker` or `systemctl` command that changes state, unless asked.
- Run only the light checks listed under "Checks" in AGENTS.md.
- Keep the wrap-up short: files changed, the key decision, and what the owner should run to verify,
  e.g. `kgr up` then `kgr status`.
