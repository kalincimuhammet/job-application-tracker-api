# Bewerbungs-API

Kleine REST-API in Go zur Verwaltung von Bewerbungen. Die Authentifizierung erfolgt
über JWTs von Zitadel; die Daten werden pro Benutzer als JSON-Datei gespeichert.

## Voraussetzungen

- Go 1.26 oder neuer
- Eine Zitadel-Anwendung mit JWT-Access-Tokens

## Lokal starten

PowerShell:

```powershell
$env:ZITADEL_ISSUER = "https://<deine-instanz>.zitadel.cloud"
$env:ZITADEL_AUDIENCE = "<deine-audience>"
$env:ZITADEL_REQUIRED_ROLE = "<optional>"
go run .
```

`ZITADEL_REQUIRED_ROLE` ist optional. `ADDR` und `DATA_DIR` können ebenfalls per
Umgebungsvariable gesetzt werden; Standardwerte sind `127.0.0.1:8080` und `./data`.

## Endpunkte

- `GET /api/applications` - Bewerbungen lesen
- `POST /api/applications` - komplette Liste ersetzen
- `PATCH /api/applications/:id` - einzelne Bewerbung ändern
- `DELETE /api/applications/:id` - einzelne Bewerbung löschen

Alle Endpunkte außer dem CORS-Preflight benötigen einen gültigen
`Authorization: Bearer <token>`-Header.

## Datenschutz

Die Dateien unter `data/*.json` enthalten lokale Bewerbungsdaten und werden deshalb
nicht veröffentlicht. Vor einem öffentlichen Push immer prüfen, dass keine echten
Daten, Tokens oder Zugangsdaten im Commit oder in der Git-Historie enthalten sind.

# Job Application Tracker – API

Backend for a small web app that helps job seekers track their applications: company address, application channel and date, salary expectation, rejection date, and free-text notes.

Written in Go, using only the standard library plus JWT/JWKS verification. Deployed as a native systemd service on an Oracle Cloud instance (no containers).

## Features
REST API: GET / POST / PATCH / DELETE on /api/applications
Per-user data isolation — each authenticated user's applications are stored in their own JSON file, keyed by their Zitadel sub claim
Authentication via Zitadel (OIDC), JWT verified against Zitadel's JWKS endpoint
Authorization via Zitadel project roles
Atomic writes (write-to-temp-file + rename) so a crash mid-write can't corrupt stored data
CORS handling for the Angular frontend
## Stack
Go (standard library net/http, no framework)
golang-jwt/jwt + MicahParks/keyfunc for JWT/JWKS verification
Data storage: flat JSON files (one per user) — no database, deliberately kept simple for the scope of this project
Deployment: systemd unit on Oracle Linux 9, Caddy as reverse proxy for automatic HTTPS

## Running locally
```bash
go build -o app .
ADDR=127.0.0.1:8080 \
DATA_DIR=./data \
ZITADEL_ISSUER=https://your-instance.zitadel.cloud \
ZITADEL_AUDIENCE=<your-project-resource-id> \
ZITADEL_REQUIRED_ROLE=<role-name> \
./app
```
## Configuration
Env var	Description
ADDR	Listen address (default 127.0.0.1:8080)
DATA_DIR	Directory for per-user JSON files (default ./data)
ZITADEL_ISSUER	Zitadel instance URL
ZITADEL_AUDIENCE	Zitadel project resource ID / client ID
ZITADEL_REQUIRED_ROLE	Project role required to access the API (optional — omit to only require a valid token)

## Notes on this project

Built with Claude Code as a pair-programming partner — the code itself came together quickly and cleanly; the harder part was operational (systemd permissions, remembering to actually stop-and-replace the running binary on each deploy). See commit history for the incremental build-up (GET/POST → PATCH/DELETE → auth → per-user storage).
