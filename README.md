# photo-vote

A small self-hosted website for getting honest opinions on photos. One owner uploads photos, and people they've chosen log in and rate each photo from 1 to 5 stars.

- **Login through your OIDC provider.** There are no accounts in the app and no sign-up. Two groups decide who can vote and who owns the site.
- **Pure Go and server-rendered HTML.** No JavaScript, no frontend build step, and one small binary with SQLite built in.
- **Photos are cleaned on upload.** Every image is decoded, resized and re-encoded, which removes metadata such as GPS location.
- **Results show only averages and counts.** Voters see them when a poll closes, and the owner can see them while it's open.

## Try it

```bash
go run ./cmd/devserver
```

Open <http://localhost:8080> and pick a fake user. It uses generated pictures, and throws everything away on exit.

## How a poll works

1. The owner creates a draft under `/admin`, uploads photos, and can add a short description to each.
2. The owner opens the poll. Photos can't be added or removed from then on, because ratings depend on them.
3. Voters rate any photos they like, and can change their ratings until the poll closes.
4. The owner closes the poll, and voters see the results. It can be reopened.
5. The owner deletes the poll, which permanently removes its photos and ratings.

## Is voting anonymous?

**On the site, yes. From whoever runs it, no.** People have to be able to change their ratings, so ratings stay linked to them through a keyed hash, `HMAC(secret, poll ID + sub)`. Whoever has the secret and the user list could work out who rated what. Watching the live totals change as someone votes could reveal it too. The voting page says this plainly.

What the design does do:

- No page shows who gave which rating, or a breakdown by score
- Logs never hold a voter and a rating together, and contain only the route pattern, status and duration
- A voter's `sub` is held in memory only, and is never written to disk
- Ratings have no timestamps, and rows are stored in hash order, so the data doesn't show who voted when
- The hash differs per poll, so a database copy alone can't link one voter across polls

## Configuration

Set through environment variables. The values here are examples.

| Variable | Required | Example | Notes |
|---|---|---|---|
| `BASE_URL` | Yes | `https://vote.example.com` | Must be HTTPS, except on localhost |
| `OIDC_ISSUER` | Yes | `https://auth.example.com/application/o/photo-vote/` | Must match the provider's issuer exactly, including any trailing slash |
| `OIDC_CLIENT_ID` | Yes | `photo-vote` | |
| `OIDC_CLIENT_SECRET` | Yes | | |
| `VOTER_GROUP` | Yes | `photo-vote-voters` | Group name in the `groups` claim |
| `OWNER_GROUP` | Yes | `photo-vote-owners` | Must differ from `VOTER_GROUP` |
| `VOTER_HASH_SECRET` | Yes | | At least 32 characters, for example from `openssl rand -base64 48`. If it changes, people can't update the ratings they already gave |
| `DB_PATH` | No | `/data/photo-vote.db` | |
| `LISTEN_ADDR` | No | `:8080` | |
| `MAX_ITEMS_PER_POLL` | No | `50` | |
| `MAX_STORAGE_MB` | No | `1024` | Total size of stored photos |

## Deploying

- **Provider:** register `<BASE_URL>/auth/callback` as the redirect URI, and have the `groups` scope add a `groups` claim to the **ID token**. The app asks only for `openid groups`, so it never sees an email or name.
- **Restrict the app at the provider too.** The app refuses anyone outside the two groups, but the provider should also bind the application to them.
- **Restrict `/admin` at your reverse proxy**, for example to your own network. The app also requires the owner login, but voters then never reach an upload handler. Images are served from `/images/`, so that stays reachable.
- **Run one replica.** Sessions are held in memory, and SQLite needs a single writer. A restart just sends people through a quick login.
- **Give it persistent storage** at the directory holding `DB_PATH`. The image runs as a non-root user and works with a read-only root filesystem. Allow about 512 MiB of memory for decoding large images, and set `GOMEMLIMIT` a little below your limit.
- **Back up if you need to.** There's no built-in backup. Note that a backup keeps photos and ratings after a poll is deleted.

Changes to group membership apply at the next login, and sessions last 12 hours at most.

Images are published to `ghcr.io/sander-skjulsvik/photo-vote` when a release is created.

## Development

See [AGENTS.md](AGENTS.md). This repo is public, so please read its rules before adding anything.
