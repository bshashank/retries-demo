# Deploying to Cloud Run

**Live instance:** https://pipeline-health-oifjodl6rq-uc.a.run.app

## Prerequisites

- A GCP project with billing enabled
- Cloud Run, Cloud Build, and Artifact Registry APIs enabled
- `gcloud` CLI authenticated (`gcloud auth login`), with the target project set
  (`gcloud config set project <PROJECT_ID>`)

## Deploy

One command builds and deploys everything. The multi-stage [Dockerfile](Dockerfile) builds
the React frontend, embeds it into the Go binary via `embed.FS`, and produces a single
distroless container — no separate hosting, no CORS:

```
gcloud run deploy pipeline-health --source . --region us-central1 \
  --allow-unauthenticated --max-instances 1 --min-instances 0
```

Re-running the same command redeploys in place (new revision, same service and URL).

## Why these flags

- `--max-instances 1` — the simulation is a single authoritative in-memory world;
  autoscaling would give concurrent viewers different, inconsistent states.
- `--min-instances 0` (the default, deliberately left unpinned) — the tick loop and load
  generator are goroutines tied to the process, not to any request (`internal/sim/engine.go`),
  and the SSE handler only reads a cached snapshot, so they behave identically whether 0 or
  many clients are connected. With nobody viewing, Cloud Run throttles the instance's CPU and
  eventually scales it to zero. There's no persistence anywhere — `Reset()` doesn't touch
  durable storage either — so a cold start looks identical to a fresh baseline. This keeps the
  service on Cloud Run's default *request-based* billing instead of paying for a 24/7 instance:
  an earlier iteration of this deploy pinned `--min-instances 1` with `--no-cpu-throttling`,
  which cost roughly $60-90/month for a demo that's realistically viewed for a few minutes at
  a time.

## Verifying a deploy

```
gcloud run services describe pipeline-health --region us-central1 \
  --format="yaml(spec.template.metadata.annotations, status.url)"
```

Confirm `maxScale: '1'`, no `minScale` annotation (defaults to 0), and
`cpu-throttling: 'true'` (the default — **not** `'false'`). Then open the URL after a few idle
minutes and confirm it cold-starts cleanly to a baseline OK state, the same as a manual Reset.

## Rolling back

Cloud Run keeps prior revisions:

```
gcloud run revisions list --service pipeline-health --region us-central1
gcloud run services update-traffic pipeline-health --region us-central1 --to-revisions <REVISION>=100
```
