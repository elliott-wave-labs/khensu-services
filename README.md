# khensu-services

khensu.nu services: key transparency log, anonymous credential issuer, push gateway, MOTOM integration (Go)

## feed: public video feed

`cmd/feed` is the shared short-video feed for khensu.nu. Public posts are plain files in the Google Cloud Storage bucket `khensu-feed`; browsers upload straight to the bucket with signed URLs and play videos from it. The server does no media processing (no transcoding, no AI) and never touches private messages.

### API

Every response is JSON except `/healthz`. CORS allows `https://khensu.nu`, `https://*.khensu.nu`, `http://127.0.0.1:4321` and `http://localhost:4321`, with `Access-Control-Allow-Headers: content-type, authorization`. JSON request bodies are limited to 16 KB (413 above that). Errors are `{"error":"..."}`.

| Method and path | Request | Response |
| --- | --- | --- |
| `GET /v1/feed?limit=20&before=<cursor>` | `limit` 1-50 (default 20) | 200 `{"posts":[Post],"next":"<cursor or empty>"}`, newest first, hidden posts left out, `Cache-Control: public, max-age=5` |
| `POST /v1/uploads` | `{"kind":"video"\|"poster","content_type":"...","bytes":n}` | 200 `{"object":"u/<random>.<ext>","upload_url":"...","public_url":"https://storage.googleapis.com/khensu-feed/u/..."}` |
| `POST /v1/posts` | `Authorization: Bearer <handle token>` (not needed for a handle's first post), `{"handle","caption","video_object","poster_object","duration_s","width","height"}` | 201 `{"post":Post,"token":"..."}`, `token` only when this request claimed the handle |
| `POST /v1/posts/{id}/report` | `{"reason":"..."}` (optional) | 204 |
| `GET /healthz` | | 200 `ok` |

`Post` is `{"id","handle","caption","video_url","poster_url","duration_s","width","height","created_at"}`; `poster_url` may be empty and `created_at` is RFC 3339.

Uploads: videos are `video/mp4`, `video/webm` or `video/quicktime` up to 150 MB; posters are `image/jpeg`, `image/png` or `image/webp` up to 5 MB. A wrong type is 415, too large is 413. `upload_url` takes one `PUT` with the same `Content-Type` header for 15 minutes.

Posts: the handle is 1-64 Unicode letters, marks, digits, `.`, `_` or `-` after NFKC, compared case-folded (`Elliott`, `ELLIOTT` and `ｅｌｌｉｏｔｔ` are one handle, shown as first claimed). The first post claims the handle and returns a random 32-byte base64url token, which the client must keep; the server stores only its SHA-256 at `handles/<folded>.json`. Later posts for that handle without the token are 403. The video object must already be uploaded (400 otherwise), and is checked for size and type. Captions are at most 2200 characters.

Reports: a post reported from 3 distinct IP hashes is hidden (`"hidden":true` in its JSON) and drops out of the feed. Reports live in the post's JSON.

### Storage layout

| Object | Contents |
| --- | --- |
| `u/<random>.<ext>` | uploaded videos and posters |
| `posts/<inverted-timestamp>-<rand>.json` | one post; the inverted nanosecond timestamp makes a prefix list newest first, and the feed cursor is the last object name |
| `handles/<folded>.json` | handle claim with the token's SHA-256 |

The bucket is public-read, so every object above can be fetched by anyone, including report reasons and salted IP hashes.

### Abuse limits

Per IP, token buckets of 20 uploads, 10 posts and 30 reports an hour (429 with `Retry-After`). IPs are hashed with HMAC under a salt that changes each UTC day (derived from `FEED_IP_SECRET`); IPv6 is counted per /64. Raw IPs are never logged or stored. The buckets live in each instance's memory, so the real limit is per instance.

### Run locally

```sh
go test ./... && go vet ./...
PORT=8787 FEED_STORE=dir:/tmp/feed-data go run ./cmd/feed
```

`FEED_STORE=mem` keeps everything in memory instead. In `mem` and `dir` mode, `upload_url` is `PUT /local/<object>` on this server and `public_url` is `GET /local/<object>` (with range requests), so a site on `localhost:4321` can run the whole flow. Set `FEED_PUBLIC_BASE` if the server is reached at another origin than `http://127.0.0.1:$PORT`.

| Variable | Default | Meaning |
| --- | --- | --- |
| `PORT` | `8080` | listen port |
| `FEED_STORE` | `gcs` | `gcs`, `mem` or `dir:/path` |
| `FEED_BUCKET` | `khensu-feed` | GCS bucket |
| `FEED_IP_SECRET` | random per process (required for `gcs`) | secret for the daily IP-hash salt |
| `FEED_XFF_HOPS` | `1` for `gcs`, `0` otherwise | which `X-Forwarded-For` entry, counted from the right, is the client; `0` uses the TCP peer |
| `FEED_PUBLIC_BASE` | `http://127.0.0.1:$PORT` | origin for `/local/` URLs |

### Deploy (Cloud Run)

```sh
deploy/feed/deploy.sh bucket   # bucket with uniform access, allUsers objectViewer, CORS for PUT/GET
deploy/feed/deploy.sh deploy   # service account, IAM, IP-hash secret, gcloud run deploy --source
deploy/feed/deploy.sh status
```

Defaults: `PROJECT=moqom-503700`, `REGION=us-central1`, `SERVICE=khensu-feed`, `BUCKET=khensu-feed`. The service runs as `khensu-feed@PROJECT.iam.gserviceaccount.com` with `roles/storage.objectAdmin` on the bucket and `roles/iam.serviceAccountTokenCreator` on itself, which lets it sign upload URLs through the IAM Credentials API without a key file. `FEED_IP_SECRET` comes from the Secret Manager secret `khensu-feed-ip-secret`. The image is a static binary on distroless (`Dockerfile`).

GCS CORS only matches exact origins, so `deploy.sh` lists each product host as well as `https://*.khensu.nu`; add new hosts there and rerun `bucket`.

### Moderation: open items for Elliott

- Public uploads need a published takedown contact: e@khensu.nu (put it on the feed page and in its terms).
- US law (18 U.S.C. 2258A) requires reporting apparent child sexual abuse material to NCMEC's CyberTipline once we know of it, and preserving the report for 90 days. There is no review queue, NCMEC registration or preservation process yet. Automatic hiding after 3 reports is not a substitute.
- Hidden posts stay in the bucket and their video URLs stay reachable; removal is a manual delete of the `u/` objects and the post JSON today.
- Uploaded objects are public as soon as they are PUT, even if never posted; a lifecycle rule or sweep for orphaned `u/` objects is not in place.
