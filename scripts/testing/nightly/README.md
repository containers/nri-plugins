# Nightly e2e test runs

`e2e-runner` runs the end-to-end test suite the way a nightly does: it fetches
the branch to test, builds it, runs the tests in the test VMs, collects what
they leave behind, reports on it and publishes the report. `e2e-report serve`
serves what it published.

This describes how to set the two up on a host. For the tests themselves, and
for the coverage they collect, see [`test/e2e/README.md`](../../../test/e2e/README.md).

## What the host needs

- `git`, GNU `make`, `go` (the version in `go.mod`), `docker` with `buildx`
- `vagrant` and `qemu-system-x86`, for the test VMs
- `ansible`, which the framework provisions the VMs with
- `tar` and `xz`, for packing up the artifacts of the tests
- disk: a clone and a worktree per run, a couple of gigabytes of VM images, and
  the published results, some 20M per run browsable or 2.5M packed

The user running it has to be able to run `vagrant`, `qemu` and `docker`.

## A run

```shell
scripts/testing/nightly/e2e-runner [options] [<e2e-tests>]
```

A run clones the remote repository if it has not been cloned yet, fetches it if
it has, adds a worktree of the branch to test, and re-executes the `e2e-runner`
of the revision under test from there. It then builds the plugins, runs the
tests, destroys the test VMs, collects the results into a directory of its own
under the result root, generates the coverage report and the report of the run,
prunes what is too old to keep, and removes the worktree.

What it defaults to, all of it overridable on the command line or from the
environment:

| variable | option | default |
| --- | --- | --- |
| `REMOTE_REPO` | `--remote` | `https://github.com/containers/nri-plugins` |
| `TEST_BRANCH` | `--branch` | `main` |
| `CLONED_REPO` | `--local` | `/opt/e2e-test/nri-plugins/nri-plugins` |
| `RESULT_ROOT` | `--results` | `/opt/e2e-test/nri-plugins/results` |
| `RESULT_NAME` | `--name` | the date and time, `2026-09-16-02-00` |
| `RETENTION_DAYS` | `--retention-days` | 100, `0` keeps everything |
| `RETENTION_KEEP` | `--retention-keep` | 3 latest runs, whatever their age |
| `KEEP_ARTIFACTS` | `--keep-artifacts` | `failed`, the rest packed per test |
| `KEEP_COVERAGE_DATA` | `--keep-coverage-data` | off, the merged data is kept |
| `PACK_RESULTS` | `--pack-results` | off, see below |
| `K8SCRI` | `--runtime` | containerd and cri-o on alternating days |
| `FULL_BUILD` | `--full-build`, `--minimal-build` | everything once a week |
| `RUN_IF_CHANGED` | `--run-if-changed` | off, test whenever asked |
| `FORCE_AFTER` | `--force-after` | `24h`, with `--run-if-changed` |

Anything left over on the command line is the test set to run, the whole default
set if there is none.

The exit status says whether the runner did its job, not whether the tests
passed: a run whose tests failed is a run which went fine. The verdict is the
first word of `status.txt` of the run, and `results.json` has the details.

**One run at a time per result root.** A run holds a lock in the root for as
long as it lasts, because two runs there would share a worktree, fight over the
test VMs and publish over each other. A second run refuses to start and says so;
with `--run-if-changed` it says there is nothing to do and exits successfully,
which is what a poll every few minutes wants. The lock is a file descriptor, so
the kernel drops it however a run ends -- there is no stale lock to clear after
a crash, and nothing to reason about a pid which may since have been given to
something else.

## From cron

Nothing here wants root, so this is the crontab of the user which owns the result
root and can run docker, vagrant and qemu, `crontab -e` as that user:

```crontab
PATH=/usr/local/bin:/usr/bin:/bin

0 2 * * * /opt/e2e-test/nri-plugins/nri-plugins/scripts/testing/nightly/e2e-runner \
              --results /opt/e2e-test/nri-plugins/results \
              >>$HOME/e2e-runner.log 2>&1
```

A drop-in in `/etc/cron.d` works as well, and takes the user to run as in a field
of its own after the five of the schedule. Name the user there: every example of
the format says `root`, and this needs none of it.

Once a run has a directory to publish into, everything it prints goes to
`e2e-runner.log.txt` there, and only what happens before that lands in the log
above. Run by hand from a terminal it prints to both.

The first run needs this script from somewhere, so start from a clone of your
own or from `CLONED_REPO`; from then on each run tests, and reports with, the
revision it fetched.

On a host behind a proxy, put the proxy variables in a file and point
`--source-proxies` at it: the runner exports them and passes them into the test
VMs.

Note that the copy of `e2e-runner` which parses the options is whichever one you
invoke -- the clone's, when cron runs it out of the clone. The runner
re-executes itself out of the worktree it makes, so the tests, the framework and
the report tool are always the branch's, but it cannot re-execute its way out of
not understanding an option the checked-out copy has never heard of. A clone left
on a revision older than `--run-if-changed` fails with `unknown command line
option`.

## Being told how a run went

To be told when a run fails, look at the verdict rather than at the exit status:

```shell
read -r verdict _ < "$RESULT_ROOT/latest/status.txt"
[ "$verdict" = PASS ] || echo "e2e run $verdict, see $RESULT_ROOT/latest/"
```

## Serving the results

`e2e-report serve` is what serves the results, and the only thing which can: the
index of the runs and the report of each run are rendered for every request, not
written out, so there are no pages under the result root for a file server to
serve.

```shell
make e2e-report
install -m 755 build/bin/e2e-report /usr/local/bin/
e2e-report serve --address :8080 /opt/e2e-test/nri-plugins/results
```

It serves a packed run as if its archive had been extracted, serves into the
tarballs of the tests as well, and reads nothing but what is under the result
root. It writes nothing at all, so it is safe for a server given the results
read-only.

Rendering every page as it is asked for is what keeps the results in step with
the tool serving them: a run published months ago is shown the way a run
published today is, without anything being migrated or rewritten, and a run still
going is listed and reported on from what it has collected so far.

What no amount of rendering can show is something a run never recorded. A run
pruned by an older runner recorded no link to its plugin log, and only reading its
results again finds one inside `artifacts.tar.xz`:

```shell
e2e-report refresh /opt/e2e-test/nri-plugins/results
```

That reports on every unpacked run under the root again. Packed runs are left
alone, their results being inside the archive.

There is a systemd unit for it next to this file:

```shell
install -m 644 scripts/testing/nightly/nri-plugins-e2e-results.service \
    /etc/systemd/system/nri-plugins-e2e-results.service
install -m 644 scripts/testing/nightly/nri-plugins-e2e-results.env \
    /etc/sysconfig/nri-plugins-e2e-results
systemctl daemon-reload
systemctl enable --now nri-plugins-e2e-results
```

The address to listen on and the result root come from
`/etc/sysconfig/nri-plugins-e2e-results`, so a host is configured without editing
the unit; the user and the group are in the unit itself, as systemd does not
expand variables there. The `.env` file is optional, and so is every setting in
it: what it leaves out the unit defaults to.

For a name and a certificate, put caddy in front of it with the configuration
next to this file, which does nothing but pass everything on:

```shell
E2E_PORT=8443 E2E_SERVER=127.0.0.1:8080 caddy run \
    --config scripts/testing/nightly/nri-plugins-e2e-results.Caddyfile
```

Keep the server's own address on the loopback interface as it comes: that is the
only place the restriction lives, and caddy binds every interface on purpose.

`make e2e-report` builds it to `build/bin`, and nothing else does: it is not part
of any image and not one of the binaries a release ships. Build it again when the
results it serves start coming from a newer revision. `go run
./test/e2e/cmd/e2e-report serve ...` from a checkout works just as well.

## Packing up a run

A run of the whole suite collects some 120M and publishes 20M of it, having
pruned and packed the rest away. `--pack-results` publishes all of it in a
single `results.tar.zst` of some 2.5M instead, keeping the artifacts of every
test and the coverage data of each:

```crontab
0 2 * * * /opt/e2e-test/nri-plugins/nri-plugins/scripts/testing/nightly/e2e-runner \
              --results /opt/e2e-test/nri-plugins/results --pack-results \
              >>$HOME/e2e-runner.log 2>&1
```

`results.json`, `status.txt` and `summary.txt` stay where they are, so how a run
went is readable without unpacking anything and its report renders in full. The
logs, the command transcripts and the coverage report are all in the archive.
That takes `e2e-report serve`, so set that up first. `tar --zstd -xf` gets the
results of a run out without a server.

Packing is the last thing a run does, after the verdict, because the log of the
runner is packed with the rest and anything written to it afterwards would go
nowhere. So the log in the archive is the whole of it, down to the verdict; the
only thing missing is what packing itself reports. That is also why the runner
builds `e2e-report` out of the worktree into a directory of its own rather than
running it with `go run`: by the time it packs, the worktree is gone.

## What a run publishes

```text
<result root>/latest -> <newest run>
              <run>/results.json            what the run collected, which its
                                            report is rendered from
                    status.txt              PASS 55/55 tests passed
                    summary.txt             a line per test case
                    git.describe, git.sha1, git.remote, git.branch
                    git.runner              the tree the runner came from
                    e2e-runner.log.txt      the log of the run
                    coverage-report/        profile, browsable report, summary
                    <topology-distro-runtime>/policies.test-suite/<policy>/<test>/
                    results.tar.zst         all of the above, with --pack-results
```

A run records where it came from as well as how it went: `git.remote` and
`git.branch` are the repository and the branch it was picked up from,
`git.describe` and `git.sha1` the revision it tested of them, and `git.runner`
the tree the runner script itself came from. That last one is normally the
revision under test, since the runner re-execs itself out of the worktree it
creates, so a report mentions it only when the two differ, which happens for a
run driven by hand or with `--skip-worktree`. It is left out altogether when
there is no telling, as for a runner read straight out of a repository with
`git show`.
