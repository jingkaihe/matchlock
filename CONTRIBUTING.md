# Contributing to Matchlock

Thanks for your interest in contributing to Matchlock! Here's how to get involved.

## Getting Started

- Small bug fixes - go ahead and open a PR directly.
- New features or architecture changes - please open an issue or start a discussion first so we can align on the approach before you invest time in code.

## Running Tests

Before submitting a PR, make sure the test suites pass:

```bash
mise run test              # Unit tests
mise run test:acceptance   # Acceptance tests (KVM/Firecracker on Linux, QEMU TCG fallback, or Virtualization.framework on macOS). Set MATCHLOCK_BACKEND to select a Linux backend.
```

You only need to run acceptance tests on the platform you're developing on.

> **Note:** Acceptance tests on Linux should pass reliably. macOS acceptance tests are known to be slightly flaky - a few sporadic failures are expected and won't block your PR.

## AI-Assisted PRs

AI-assisted or AI-coded contributions are welcome, provided you follow these guidelines:

- [ ] PR title and description are written with a human touch
- [ ] Disclose AI usage (including the prompt and conversation history is super useful)
- [ ] You understand what the code does
- [ ] Keep the scope focused - fix or deliver one thing per PR
- [ ] Code is tested to some degree, and you have manually verified it works

**Bot-raised PRs will be closed. Repeated offences may result in the account being blocked.**
