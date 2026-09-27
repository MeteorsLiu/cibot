# cibot

Automation for LLAR Hub. The repository currently contains the command and package skeleton only.

Build the command with `go build -o ci ./cmd/ci`. The planned entry point is `ci serve`; the server and worker are not implemented yet.

Local deployment settings belong in `.env`, which is ignored by Git. The required settings will be defined alongside the server implementation.
