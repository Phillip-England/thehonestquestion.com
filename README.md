# The Honest Question

A Go website with a private Markdown editor for posts.

## Set up and run

Create `config/.env` with your own values:

```dotenv
ADMIN_USERNAME=your-admin-name
ADMIN_PASSWORD=replace-with-a-long-unique-password
SESSION_SECRET=replace-with-a-random-secret-at-least-32-characters
PORT=8080
```

Run `make run` and open <http://localhost:8080/admin>. The credentials are required; the site will not start without them. Set `PORT` only if you need a different local port. The Docker image exposes port 8080, so keep that port for Uppr.

Write a title, hashtags such as `#faith #questions`, a summary, and a Markdown body. You can also upload an image for the post; FFmpeg converts it to a 1200 × 675 JPEG thumbnail automatically. JPG, PNG, WebP, GIF, HEIC, and other formats supported by FFmpeg work, with a 20 MB upload limit. Replace or remove the image from the editor. The preview updates while you type. Save as a draft or check **Publish this post** to make it appear on the public site. Existing articles are copied into the database on first start and can then be edited or deleted in the admin area. Readers can search posts and filter them by hashtag on the home page. Existing category labels are converted to hashtags when the database is upgraded.

Posts, login sessions, contact messages, and abuse counters are stored in `data/main.sqlite` (mode `0600`). Five failed sign-ins from one IP within 24 hours block further attempts until the oldest attempt expires. Sessions expire after 12 hours, and expired rows are removed on startup or login. Generate `SESSION_SECRET` with a secure random source, keep it private, and keep it stable across restarts. Changing it signs out existing sessions.

Contact notes are visible only to signed-in admins at `/admin/messages`. The contact form has a hidden bot trap and allows at most three accepted notes per IP in a rolling 24-hour window. Old counter rows are purged during checks. Notes are retained for up to one year, with only the newest 1,000 kept. On startup, an existing `data/questions.jsonl` is imported into SQLite once and removed after a successful import. Back up `data/` before upgrading if you need a separate copy of older notes.

By default, IP limits use the direct connecting address. If the app is behind a reverse proxy, set `TRUSTED_PROXY_CIDRS` to that proxy's exact IP or network, and configure it to append or replace `X-Forwarded-For`. Only trusted peers' forwarded addresses are used. Keep direct access to the app restricted to the proxy; configure edge rate limiting there too.

Converted post images are stored in `data/uploads/`. Back up the whole `data/` directory to preserve posts, images, and contact notes. `config/` and `data/` are ignored by Git and excluded from the Docker image.

## Uppr

The Docker image includes FFmpeg and a HEIC decoder, and listens on `0.0.0.0:8080`. For local runs, install FFmpeg and ensure `ffmpeg` is on your `PATH`. For HEIC uploads locally, also install `heif-convert`. Uppr mounts `config/` at `/app/config` and `data/` at `/app/data`. The app loads `/app/config/.env` and uses `/app/data/main.sqlite` inside the container. The container runs as UID `65532`; mounted `config/.env` must be readable by that UID, and `data/` must be writable by it. Set ownership on copied deployment files before starting the container so their private `0600` modes remain effective. `schema.json` describes every supported setting.

## Tests

```sh
make test
```
