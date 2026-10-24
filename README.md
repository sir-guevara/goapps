# G&O Apps

A self-hosted app launcher. Everyone can browse, search, filter, and open sites. Only the signed-in admin can add, edit, or delete sites, enforced by the server on every management request. There is no public registration.

## Run

Requires Go 1.22 or newer. The SQLite driver uses pure Go; no C compiler or external database server is required.

```sh
go mod tidy
ADMIN_USERNAME=admin ADMIN_PASSWORD='choose-a-strong-password' go run .
```

Open `http://localhost:5001`, choose **Admin sign in**, and use those credentials. Set `ADDR` to change the listen address. Use HTTPS when deploying publicly so session cookies are sent securely.

The admin account is created only on the first run and stored in SQLite with a bcrypt password hash. If `ADMIN_PASSWORD` is unset on that first run, the server generates and prints an initial password; save it. Passwords must be at most 72 bytes. Later restarts use the saved account; changing environment variables does not overwrite it.

Sites, click counts, the admin account, and sessions are stored in `data/commonplace.db`. Sessions expire after 12 hours, survive restarts, and are revoked on sign out. Only hashes of session tokens are stored in the database. Cookies are HttpOnly and SameSite=Lax. Cross-site mutation requests are rejected.

An existing `data/bookmarks.json` is imported automatically once, preserving sites and click counts. The JSON file is left intact. Fresh installations start with example sites. Deleting every site leaves an empty list across restarts.

Back up SQLite using its backup API or stop the server before copying `data/commonplace.db` (including any `-wal`/`-shm` files still present). Keep the data directory private.

## Site icons and previews

Icons and preview images are fetched automatically in the background when sites are added, when their URLs change, and for existing sites on startup. Cards update as images become available. Favicons are discovered from the page with a `/favicon.ico` fallback. Previews use the site's Open Graph or Twitter image, rather than a rendered screenshot; pages without a usable image show a domain placeholder.

Images are stored in SQLite alongside each bookmark and served locally, so visitors do not contact the original image hosts. Deleting a bookmark cascades to its cached images in the same transaction. Changing its URL clears the old cache; in-flight fetches cannot restore images for a deleted bookmark or an outdated URL. Incomplete fetches retry after 24 hours, and unavailable images retain the letter/domain fallback. Existing images survive restarts and temporary fetch failures.

Fetching is limited to public HTTP/HTTPS addresses on ports 80/443, with DNS and redirect checks, timeouts, and response size limits. Local or private-network sites still work as links but keep fallback artwork. No external screenshot service or browser is needed.

## Verify

```sh
go test ./...
go vet ./...
```
