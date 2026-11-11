<h1 align="center">awsport</h1>

<p align="center">
  <b>From a fresh laptop to <code>localhost:15432</code> in one command.</b><br>
  A terminal UI and CLI for AWS Systems Manager tunnels, with SSO onboarding and stable, conflict-free local ports.
</p>

<p align="center">
  <a href="https://github.com/nhl4000/awsport/actions/workflows/ci.yml"><img alt="CI" src="https://github.com/nhl4000/awsport/actions/workflows/ci.yml/badge.svg"></a>
  <a href="https://github.com/nhl4000/awsport/releases"><img alt="Release" src="https://img.shields.io/github/v/release/nhl4000/awsport?sort=semver"></a>
  <img alt="Go" src="https://img.shields.io/badge/go-1.27-00ADD8?logo=go&logoColor=white">
  <img alt="Platforms" src="https://img.shields.io/badge/platforms-macOS%20%7C%20Linux%20%7C%20Windows-555">
  <a href="LICENSE"><img alt="License: MIT" src="https://img.shields.io/badge/license-MIT-blue"></a>
</p>

<p align="center">
  <img src="assets/tui.gif" alt="awsport TUI: pick a profile, open tunnels to an EC2 port and an RDS database, view the tunnel dashboard" width="900">
</p>

## Why

Reaching private resources through SSM usually means a folder of scripts like
this, one per server and per port:

```sh
INSTANCE_ID=$(aws ec2 describe-instances --filters "Name=tag:Name,Values=my-app" ... --output text)
aws ssm start-session --target "$INSTANCE_ID" \
  --document-name AWS-StartPortForwardingSession \
  --parameters '{"portNumber":["8025"],"localPortNumber":["18025"]}'
```

…plus remembering which local port belongs to which database, fighting port
clashes when two tunnels default to `3306`, and walking every new teammate
through AWS CLI, plugin and SSO setup. awsport replaces all of that.

## Features

- 🧭 **Guided onboarding** — checks for the AWS CLI v2 and the Session Manager plugin and offers to install them; sets up IAM Identity Center (SSO) and writes one AWS profile per account/role you can access.
- 🔎 **Discovery** — lists EC2 instances and RDS/Aurora databases per profile, across regions; unreachable ones are hidden with the reason (stopped, SSM agent offline…).
- 🔌 **Tunnels** — SSM port forwards to EC2 ports, and to RDS through a bastion that is found automatically.
- 📌 **Stable, conflict-free ports** — `local = 10000 + remote`, remembered per resource, so your DB client bookmarks keep working.
- 🐚 **Shells** — an interactive SSM shell on any instance, without leaving the TUI.
- 📦 **Groups** — start a whole dev stack (app + database + mailpit) with one key or one command.
- ♻️ **Self-healing** — dropped tunnels reconnect with backoff; expired SSO sessions prompt an inline re-login and resume on the same ports.
- 🧰 **Scriptable** — `awsport up <group>` runs tunnels in the foreground for scripts and CI-like workflows.

<p align="center">
  <img src="assets/cli.gif" alt="awsport CLI: init from a team config, SSO login, list resources, start a tunnel group" width="900">
</p>

> **Unofficial — not affiliated with or endorsed by Amazon Web Services.**
> "AWS" and related names are trademarks of Amazon.com, Inc. or its affiliates.

## Install

macOS / Linux:

```sh
curl -fsSL https://raw.githubusercontent.com/nhl4000/awsport/main/install.sh | sh
```

The script detects OS/architecture, downloads the release archive, verifies its
SHA-256 checksum against `checksums.txt` and installs to `~/.local/bin`
(`/usr/local/bin` when run as root). Options via environment:

| Variable          | Meaning                                                     |
| ----------------- | ----------------------------------------------------------- |
| `AWSPORT_VERSION` | release tag to install, e.g. `v0.3.0` (default: latest)     |
| `INSTALL_DIR`     | target directory                                            |
| `GITHUB_TOKEN`    | token for a private fork / higher GitHub API rate limits    |

Windows: download `awsport_windows_amd64.zip` (or `arm64`) from the
[releases page](https://github.com/nhl4000/awsport/releases) and put
`awsport.exe` on your `PATH`.

From source: `go install github.com/nhl4000/awsport/cmd/awsport@latest`.

Update later with `awsport update`.

## Getting started

Run `awsport`. On first start it walks you through onboarding:

1. **Prerequisites** — AWS CLI v2 (2.9 or newer) and `session-manager-plugin`. Missing tools
   come with the official install commands for your OS, which awsport can run
   for you.
2. **SSO setup**, one of:
   - import a team config (`awsport init --from <url|path>`), which usually
     contains the SSO start URL and region;
   - reuse an existing `[sso-session]` already in `~/.aws/config`;
   - enter the start URL (e.g. `https://d-0000000000.awsapps.com/start`) and region.
3. **Login** — awsport runs `aws sso login`, shows the sign-in link and code,
   and opens your browser (on WSL via `wslview` or Windows directly). If no
   browser can be opened, click or copy the link instead.
4. **Profile sync** — awsport lists every account and role you can access and
   writes one profile per pair, named `<account-name>-<role-name>`, into a
   clearly marked block in `~/.aws/config`:

   ```ini
   # >>> awsport managed (do not edit) >>>
   [sso-session awsport]
   ...
   [profile example-dev-developer]
   ...
   # <<< awsport managed <<<
   ```

   Nothing outside the block is touched, and the original file is backed up to
   `~/.aws/config.awsport.bak` before each write. Existing profiles (SSO,
   assume-role, static keys) keep working and show up in awsport too.

The same steps without the TUI:

```sh
awsport init --start-url https://d-0000000000.awsapps.com/start --region us-east-1
awsport login
awsport ls
```

## Tag conventions

awsport works with untagged resources, but tags make it smarter:

| Tag               | On  | Value                       | Effect                                                        |
| ----------------- | --- | --------------------------- | ------------------------------------------------------------- |
| `awsport:ports`   | EC2 | `mailpit:8025,app:8080`     | ports offered for this instance (in addition to presets)      |
| `awsport:bastion` | EC2 | `true`                      | use this instance to reach RDS in its account/region          |
| `awsport:shell`   | EC2 | `sudo su - ubuntu`          | command run when opening an interactive shell                 |

Bastion resolution for RDS tunnels, per profile and region: an instance tagged
`awsport:bastion=true` (running, SSM online) → an instance named
`management-bastion` → the bastion you picked earlier → otherwise awsport asks
you to pick one of the SSM-online instances and remembers the choice.

Instances need the SSM agent and an instance profile allowing SSM
(e.g. `AmazonSSMManagedInstanceCore`). Instances that can't be reached are
hidden by default; press `a` in the TUI (or `awsport ls --all`) to see them
with the reason.

## Local ports

Each tunnel gets a local port derived from the remote port:
**local = 10000 + remote** (e.g. remote `5432` → `127.0.0.1:15432`; remote
ports above 55535 are used as-is).

- Ports are **stable**: the assignment is saved per profile + region +
  resource name + remote port, so the same database is always on the same
  local port — even after the instance is replaced.
- Ports are **conflict-free**: when the default is taken by another saved
  tunnel, the next free port is picked and saved.
- If a saved port is busy at connect time (another program holds it), awsport
  uses a **temporary** port for this session only and marks it with `*`; the
  saved port is kept for next time.

## Tunnels that outlive the window

Tunnels run inside a small background **agent** (`awsport agent`, started
automatically with the first tunnel). Quitting the TUI with tunnels active asks:

- **`k` keep running** (or press `q` again) — awsport closes, tunnels stay up (and keep
  auto-reconnecting). Reopen `awsport` and they're listed with live status.
- **`s` stop all** — tunnels are closed, then awsport quits.
- **`esc` cancel**.

The agent exits by itself 30 seconds after the last tunnel is gone. From a
shell: `awsport ps` lists running tunnels, `awsport down <name>` (or
`--all`) stops them. The agent listens on a user-only socket in the state
directory and logs to `agent.log` next to it.

In the TUI, `enter` on a port toggles its tunnel on and off (`x` also stops).
Web ports show their local address as a clickable link (in terminals that
support OSC 8 hyperlinks: iTerm2, Windows Terminal, VS Code, GNOME Terminal,
WezTerm, kitty…); `o` opens it in your browser and `c` copies the address.

## Configuration

| File                                    | Written by      | Contents                                   |
| --------------------------------------- | --------------- | ------------------------------------------ |
| `~/.config/awsport/config.yaml`         | you / `init`    | personal settings (override the team file) |
| `~/.config/awsport/team.yaml`           | `init --from`   | imported team config                       |
| `~/.local/state/awsport/state.json`     | awsport         | saved ports, bastions, manual ports, groups |

On macOS and Windows both live under the OS config directory
(`~/Library/Application Support/awsport`, `%AppData%\awsport`). Override with
`$AWSPORT_CONFIG_DIR` / `$AWSPORT_STATE_DIR`.

### Team config

Share one file so everyone gets the same SSO portal, regions, port presets and
tunnel groups. See [`examples/awsport.team.yaml`](examples/awsport.team.yaml):

```yaml
sso:
  start_url: https://d-0000000000.awsapps.com/start
  region: us-east-1
  session: awsport
regions: [us-east-1, us-west-2]
presets:
  - { name: mailpit, remote: 8025 }
  - { name: app, remote: 8080 }
shell_command: sudo su - ubuntu
reconnect: true
max_failures: 5
groups:
  - name: example-dev-stack
    members:
      - { profile: example-dev-developer, kind: ec2, name: example-app, remote: 8080 }
      - { profile: example-dev-developer, kind: rds, name: example-db, region: us-east-1, remote: 5432 }
```

Import it with `awsport init --from https://example.com/awsport.team.yaml`
(or a file path). Your own `config.yaml` wins over the team file; presets and
groups are merged by remote port / name.

Group members match resources by kind + name (EC2 `Name` tag or RDS
identifier) and, if set, region — so groups survive instance replacement.

## CLI reference

| Command                                         | Description                                                                 |
| ----------------------------------------------- | --------------------------------------------------------------------------- |
| `awsport`                                       | start the TUI (onboarding on first run)                                     |
| `awsport init --from <url\|path>`               | import a team config                                                        |
| `awsport init --start-url <url> --region <r> [--session <name>]` | configure SSO                                              |
| `awsport init`                                  | adopt the first `[sso-session]` from `~/.aws/config`                        |
| `awsport login`                                 | SSO login, then sync profiles into `~/.aws/config`                          |
| `awsport login <profile>`                       | `aws sso login --profile <profile>`                                         |
| `awsport ls [profile] [--all] [--json]`         | list EC2/RDS resources (all profiles when none given)                       |
| `awsport up <group \| profile/name[:port]>... [-d]` | start tunnels; prints `127.0.0.1:<port>`; Ctrl-C stops the ones it started, `-d` leaves them running in the background |
| `awsport ps [--json]`                           | list tunnels running in the background agent                                |
| `awsport down [--all] [name\|group...]`         | stop background tunnels by label, key or group name                          |
| `awsport groups`                                | list tunnel groups                                                          |
| `awsport update`                                | self-update from the latest GitHub release (checksum-verified)              |
| `awsport version`                               | print version                                                               |

`up` targets: `example-dev-developer/example-db` (RDS, database port),
`example-dev-developer/example-app:8080` or `example-dev-developer/example-app:mailpit`
(EC2, remote port number or port name), or a group name.

## IAM permissions

The role you log in with (for IAM Identity Center: the **permission set**)
needs:

| Action                            | Used for                                  |
| --------------------------------- | ----------------------------------------- |
| `ec2:DescribeInstances`           | listing instances and tags                |
| `ssm:DescribeInstanceInformation` | SSM agent online status                   |
| `rds:DescribeDBInstances`         | listing RDS instances                     |
| `rds:DescribeDBClusters`          | listing Aurora clusters                   |
| `ssm:StartSession`                | tunnels and shells                        |
| `ssm:TerminateSession`, `ssm:ResumeSession` | closing / resuming your sessions |

Profile sync (listing accounts and roles) uses your SSO login itself and needs
no extra IAM permission.

When something is missing, awsport says which action and which permission set,
e.g. *Can't list RDS databases in ca-central-1 — missing
rds:DescribeDBInstances*. Paste this as an inline policy on the permission set
(IAM Identity Center → Permission sets → *set* → Inline policy) to cover
everything awsport does:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "AwsportDiscovery",
      "Effect": "Allow",
      "Action": [
        "ec2:DescribeInstances",
        "ssm:DescribeInstanceInformation",
        "rds:DescribeDBInstances",
        "rds:DescribeDBClusters"
      ],
      "Resource": "*"
    },
    {
      "Sid": "AwsportSessions",
      "Effect": "Allow",
      "Action": "ssm:StartSession",
      "Resource": [
        "arn:aws:ec2:*:*:instance/*",
        "arn:aws:ssm:*::document/AWS-StartPortForwardingSession",
        "arn:aws:ssm:*::document/AWS-StartPortForwardingSessionToRemoteHost",
        "arn:aws:ssm:*::document/AWS-StartInteractiveCommand"
      ]
    },
    {
      "Sid": "AwsportOwnSessions",
      "Effect": "Allow",
      "Action": ["ssm:TerminateSession", "ssm:ResumeSession"],
      "Resource": "arn:aws:ssm:*:*:session/*"
    }
  ]
}
```

Narrow `arn:aws:ec2:*:*:instance/*` (e.g. with an `ssm:resourceTag` condition)
if users should only reach some instances.

## Development

```sh
go build ./...
go vet ./...
go test ./...
```

Releases are built by GoReleaser when a `v*` tag is pushed.

The README GIFs are recorded with [VHS](https://github.com/charmbracelet/vhs)
against a fake AWS endpoint (`demo/fakeaws`) and a fake `aws` binary, so no real
account is touched. Regenerate them with `demo/record.sh` (needs `vhs`, `ttyd`
and `ffmpeg`).

## License

[MIT](LICENSE)
