# Contributing to Vortex

Thanks for contributing.

## Architecture

Vortex is an onion-architecture engine. The extension surface is a
set of Go interfaces:

- pkg/interfaces/ → Provider, AutonomousProvider, SignalFieldInterface, etc.
- store/interfaces.go → ITaskBackend, IExperienceBackend, and other backend seams.

Iron law: core defines the interface; external implementations (open or
closed) fill it in. Core must never import such implementations. Add new
capabilities as interfaces here, never as a baked-in concrete implementation of
a private feature.

## Prerequisites

- Go 1.25+

## Build and test

    go build ./...
    go test ./...

## Reporting issues

Open an issue describing the bug or feature request, with a minimal repro when
possible. For security issues, see SECURITY.md (do not file a public issue).

## Contributor License Agreement (DCO)

By contributing, you agree that your contributions are licensed under the
Apache 2.0 license, and that you grant the project maintainers the right to
re-license your contributions in future editions. If you do not agree, do not
submit a pull request.

To signify agreement, add `Signed-off-by: Your Name <your.email@example.com>`
to your commit messages (use `git commit -s`).
