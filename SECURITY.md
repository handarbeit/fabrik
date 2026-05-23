# Security Policy

## Reporting a vulnerability

Please **do not** open a public issue for security vulnerabilities.

Use GitHub's private vulnerability reporting:

1. Go to <https://github.com/handarbeit/fabrik/security/advisories/new>
2. Fill in a description of the issue, the impact, and (if known) a suggested fix or workaround.

We'll acknowledge receipt within 5 business days and aim to provide an initial assessment within 10 business days. Once a fix is available, we'll coordinate disclosure with you and publish a GitHub Security Advisory.

## Scope

In-scope:

- The `fabrik` binary and all code under this repository
- The default stage YAML configs and embedded plugin skills
- Documented configuration paths (`.fabrik/config.yaml`, `.env`, stage YAML)

Out of scope:

- Vulnerabilities in dependencies — please report those to the upstream project (we'll happily help triage if you're unsure)
- Issues in user-provided custom stage YAML, custom skills, or third-party plugins
- Misconfiguration in a user's GitHub Project board or token scopes

## Supported versions

Fabrik is pre-1.0 and ships from `main`. Security fixes are applied to the latest tagged release; older releases are not patched. If you're running an older binary, the fix is to upgrade.

## Handling of secrets

Fabrik reads a GitHub token from `FABRIK_TOKEN`, `GITHUB_TOKEN`, or a gitignored `.env` file. The engine refuses to start when `.env` exists in the working directory but is not listed in `.gitignore`. Tokens are never written to logs, comments, or PR bodies — if you observe one leaking, that's a vulnerability and we want to know.
