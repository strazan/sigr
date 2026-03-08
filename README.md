# sigr

Terminal UI for watching GitHub pull requests. Polls checks, reviews, and comments so you don't have to.

## Install

```sh
# Homebrew (macOS/Linux)
brew install strazan/tap/sigr

# Shell script
curl -fsSL https://raw.githubusercontent.com/siggelabor/sigr/main/install.sh | sh

# From source
make install
```

Requires [`gh`](https://cli.github.com/) (GitHub CLI) to be installed and authenticated.

## Usage

```sh
# Watch the PR for the current branch
sigr

# Watch a specific PR
sigr 42

# List your open PRs, pick one to watch
sigr list

# Create a PR and start watching it
sigr pr --fill
```

### Watch view

Shows CI checks, review status, and new comments with live polling.

```
#42 Add user auth (feat/auth)

 ● lint
 ● 3/4 checks successful

 ● Approved
   └ reviewer1

polling every 10s · q to quit · f fix
```

Checks auto-collapse after passing. When all checks pass and the PR is approved, `sigr` exits with code 0 (or 1 if any checks failed).

**Keys:** `q` quit · `f` fix failed checks · `esc` back to list

### List view

Shows all your open PRs with check and review summaries.

**Keys:** `↑/↓` navigate · `enter` select · `q` quit

### Fix mode

When checks fail, press `f` to enter fix mode. This uses the [Claude CLI](https://docs.anthropic.com/en/docs/claude-code) to:

1. Fetch failure logs from the failed run
2. Analyze the root cause
3. Apply a fix
4. Show you the diff for review
5. Commit and push on confirmation

**Keys:** `f` fix · `y` commit & push · `n` discard · `esc` back

## Configuration

| Variable | Default | Description |
|---|---|---|
| `SIGR_INTERVAL` | `10` | Polling interval in seconds |

