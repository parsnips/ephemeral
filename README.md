# Ephemeral Twisp tenants for heavy local tests

Twisp local (`bazel run //services/db/server:server_public`) is great for fast
iteration, but some tests need a real Twisp on real DynamoDB to drive the
throughput we want. This repo bridges the gap: `docker compose up` vends a
fresh cloud tenant, `docker compose down` deletes it, and in between everything
on `localhost:8080` / `localhost:8081` looks just like twisp-local.

```
┌─────────────┐    docker compose up    ┌──────────────────────┐
│ your tests  │  localhost:8080 (HTTP)  │  proxy   ──────────┐ │
│ + services  │  localhost:8081 (gRPC)  │                    │ │
└─────────────┘ ───────────────────────►│  (drop-in replaces │ │
                                        │   twisp-local)     │ │
                                        └────────────────────┼─┘
                                                             ▼
                                              api.<region>.<env>.twisp.com
                                              (the ephemeral tenant)
```

## What you get

| File / dir            | What it is                                                                  |
|-----------------------|------------------------------------------------------------------------------|
| `auth/`               | Reusable `http.RoundTripper` and gRPC `PerRPCCredentials` driven by `sts:GetWebIdentityToken`. |
| `vend/`               | Library for `admin.createTenant` / `admin.deleteTenant` / bootstrap `auth.createClient`. |
| `cmd/proxy/`          | Single binary. Vends a tenant on startup, runs the HTTP+gRPC drop-in proxy, reaps the tenant on SIGTERM. |
| `docker-compose.yml`  | One-service compose wrapping the above.                                     |
| `policy.example.json` | Sample policy if you need to bootstrap the new tenant's auth client by hand. |

## How the auth works

The auth flow is [AWS IAM Outbound Identity
Federation](https://gist.githubusercontent.com/parsnips/b77f3a3d2fb4f8087e55c6d1ce18ed53/raw/7c2ff6a0e4d7583afff0ae40148ef89665cbbe64/aws-outbound-identity.md):

1. Your machine (or container) calls `sts:GetWebIdentityToken` with whatever
   AWS credentials are around. AWS returns a short-lived (~5 min) JWT signed by
   STS, with `iss = https://<aws-account-uuid>.tokens.sts.global.api.aws` and
   `aud = <whatever you asked for>`.
2. We send that JWT to Twisp as `Authorization: Bearer …`. Twisp validates it
   against the JWKS at the issuer and matches the `iss` claim against a Client
   you registered on the tenant.
3. Same JWT can target any tenant in the org — pick which one with
   `X-Twisp-Account-Id`. That is why a single round tripper drives both the
   vend tenant (to call `createTenant`) and the ephemeral tenant (for actual
   workload).

Token caching, refresh, header injection, and gRPC metadata are all in the
`auth` package — your service code calls Twisp normally and the transport does
the rest.

## One-time setup

### 1. Create the vend tenant

This is just a regular tenant in your org, but its only job is to host the
`createTenant` / `deleteTenant` calls.

```graphql
mutation CreateVendTenant {
  admin {
    createTenant(input: {
      id: "00000000-0000-0000-0000-000000000001"  # any uuid
      accountId: "ephemeral-vend"
      name: "Ephemeral Vend"
      description: "Parent tenant used to spawn ephemeral test tenants"
    }) { accountId }
  }
}
```

### 2. Find your AWS STS issuer

```sh
aws sts get-web-identity-token --audience ephemeral --signing-algorithm RS256 \
  | jq -r '.WebIdentityToken' \
  | awk -F. '{print $2}' | base64 -d 2>/dev/null | jq .iss
# "https://aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee.tokens.sts.global.api.aws"
```

This issuer URL is stable per AWS account. Anyone calling
`sts:GetWebIdentityToken` from the same AWS account gets the same `iss`, so a
single Twisp client covers all your AWS principals (use `assertions` for
finer-grained checks).

### 3. Register a client on the vend tenant

Run this against the vend tenant's `/financial/v1/graphql` endpoint:

```graphql
mutation RegisterAWSClient {
  auth {
    createClient(input: {
      principal: "https://aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee.tokens.sts.global.api.aws"
      name: "ephemeral-vend-iam"
      policies: [{
        effect: ALLOW
        actions: [SELECT, INSERT, UPDATE, DELETE]
        resources: ["*"]
        assertions: {
          audIsEphemeral: "context.auth.claims.aud == 'ephemeral'"
          # Optional: restrict to a specific role
          # roleIsAllowed: "context.auth.claims.sub == 'arn:aws:iam::123456789012:role/MyRole'"
        }
      }]
    }) { principal }
  }
}
```

In Twisp, a client created on a tenant is automatically inherited by tenants
created from it via `admin.createTenant`, so you only need this one client on
the vend tenant — ephemeral children pick it up for free. If your org doesn't
behave that way, the `vend` library exposes `BootstrapClient` so the proxy can
register the client on the new tenant right after creation.

## Day to day

```sh
cp .env.example .env
# fill in VEND_ACCOUNT_ID, AWS_PROFILE, AUDIENCE if not "ephemeral"

docker compose up        # vends a tenant + starts the proxy
# point your services at http://localhost:8080 / localhost:8081 like twisp-local
docker compose down      # deletes the ephemeral tenant
```

The first run takes a beat (image build + `createTenant` round trip). Once the
proxy logs `proxy targeting tenant accountId=ephemeral-…` you're good.

## Running locally (no docker)

If you don't want to deal with compose, one shell is enough. The proxy reads
AWS credentials through the standard SDK chain — env vars, `~/.aws` profile,
SSO, instance/EKS role, whatever. Get yourself credentials in your shell
however you normally do, then:

### 1. Build (or `go run`)

```sh
cd ~/projects/twisp/ephemeral
go build -o bin/proxy ./cmd/proxy
```

### 2. Run

The process stays in the foreground. Ctrl-C runs `deleteTenant` and exits.

```sh
# whatever gets AWS creds into your env. Examples:
#   export AWS_PROFILE=my-sso-profile && aws sso login
#   eval "$(aws-vault exec my-profile -- env | grep ^AWS_)"
export AWS_REGION=us-east-1

./bin/proxy \
  -vend-account=<your-vend-tenant-accountId> \
  -audience=ephemeral
```

You'll see something like:

```
vending ephemeral tenant on parent=<your-vend-tenant>
vended ephemeral tenant accountId=ephemeral-03a1cfc19d17 id=…
proxy targeting tenant accountId=ephemeral-03a1cfc19d17
HTTP listening on :8080 -> https://api.us-east-1.cloud.twisp.com
gRPC listening on :8081 -> api.us-east-1.cloud.twisp.com:50051
```

### 3. Use it

`localhost:8080` and `localhost:8081` are the un-authenticated drop-ins.
Quick sanity check:

```sh
curl -sS http://localhost:8080/financial/v1/graphql \
  -H 'content-type: application/json' \
  -d '{"query":"{ admin { organization { name } } }"}'
```

### 4. Shut down

Ctrl-C the proxy. The signal handler calls `deleteTenant` before exit:

```
shutting down
deleting ephemeral tenant accountId=ephemeral-…
deleted  ephemeral tenant accountId=ephemeral-…
```

**Don't `kill -9`** — that skips the delete handler and leaks the tenant. If
you do leak one, run `deleteTenant` against your vend tenant by hand:

```graphql
mutation { admin { deleteTenant(accountId: "ephemeral-…") { accountId } } }
```

### Pointing at an existing tenant instead

Two flags flip the proxy out of vend mode and onto a tenant you already have:

```sh
./bin/proxy -account-id=my-existing-tenant   # static
./bin/proxy -tenant-file=/tmp/tenant.env     # read EPHEMERAL_ACCOUNT_ID=… from file
```

Useful for rerunning against a leaked tenant, or sharing one between several
proxy invocations.

### Tips

- **Audience must match the policy assertion.** If your client policy on the
  vend tenant asserts `context.auth.claims.aud == 'ephemeral'`, you must pass
  `-audience=ephemeral` (the default). A mismatch silently 403s every call.
- **`/financial/v1/graphql`, not `/graphql`.** `admin.createTenant` lives on
  the financial endpoint; the proxy already targets the right path, but if
  you're hand-rolling curl, use the longer form.
- **STS creds expire.** Most SSO sessions are 1h. The proxy refreshes the JWT
  every ~5min using whatever creds are in its environment. If your shell
  creds expire mid-test, the next refresh fails. Refresh creds in the same
  shell (`aws sso login`, etc.) and the next refresh picks them up, or
  restart the proxy.
- **One AWS account = one Twisp client.** The `iss` claim is per AWS account,
  so a single `createClient` registration on each tenant covers every IAM
  role and user in that account. Use `assertions` for finer-grained checks
  (`context.auth.claims.sub == 'arn:aws:iam::…:role/MyRole'`).

## Reusing the round tripper in your own services

```go
import (
    "net/http"

    "github.com/aws/aws-sdk-go-v2/config"
    "github.com/aws/aws-sdk-go-v2/service/sts"
    "github.com/parsnips/ephemeral/auth"
)

cfg, _ := config.LoadDefaultConfig(ctx, config.WithRegion("us-east-1"))
src := auth.NewTokenSource(sts.NewFromConfig(cfg), "ephemeral")

httpc := &http.Client{
    Transport: auth.NewRoundTripper(src, "<accountId>", nil),
}
// httpc.Post("https://api.us-east-1.cloud.twisp.com/financial/v1/graphql", ...)

// gRPC:
//   conn, _ := grpc.NewClient("api.us-east-1.cloud.twisp.com:50051",
//       grpc.WithTransportCredentials(credentials.NewTLS(nil)),
//       grpc.WithPerRPCCredentials(&auth.GRPCPerRPC{Source: src, AccountID: "<accountId>"}),
//   )
```

Same `TokenSource` instance can drive multiple tenants — just use it with
different account IDs.

## Reusing the vend library

```go
import (
    "github.com/parsnips/ephemeral/auth"
    "github.com/parsnips/ephemeral/vend"
)

v, _ := vend.New(vend.Config{
    Region:        "us-east-1",
    Env:           "cloud",
    VendAccountID: "ephemeral-vend",
    Source:        src,    // *auth.TokenSource from above
})

t, _ := v.Create(ctx)              // creates a tenant, returns Tenant{ID, AccountID}
defer v.Delete(context.Background(), t.AccountID)
// … use t.AccountID …
```

Useful if you'd rather drive create/delete from a test harness than have a
long-lived proxy do it.

## Gotchas

- **STS API version**. `sts:GetWebIdentityToken` is part of AWS IAM Outbound
  Identity Federation; your AWS SDK / CLI must be recent enough. We pin
  `aws-sdk-go-v2/service/sts >= v1.42.0`.
- **`aud` mismatch**. If your client policy asserts `context.auth.claims.aud
  == 'ephemeral'` but you launch with `AUDIENCE=foo`, every request 403s. The
  audience flag must match what the assertion expects.
- **Compose down is mandatory**. `docker compose kill` skips SIGTERM, so the
  delete-on-shutdown handler won't run and you'll leak tenants. Use `docker
  compose down` (or send SIGTERM manually). If you do leak one, just call
  `deleteTenant` from any client pointed at the vend tenant.
- **Cost**. Each ephemeral tenant is a real cloud tenant and runs real
  DynamoDB — fine for short heavy tests, painful as a permanent fixture.
- **Timeouts on stop**. Tenant deletion can take longer than compose's default
  10s `stop_grace_period`. We bump it to 60s in the compose file; tune if your
  tenants accumulate a lot of state.
