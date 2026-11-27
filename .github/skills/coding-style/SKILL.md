---
name: coding-style
description: "Use for writing, reviewing or cleaning up code, comments, commit messages, PR descriptions and documentation in nri-plugins and its e2e tests. Gives style guidelines."
---

# Code and text guidelines

## Automatic checks

- run `make reformat` to find and automatically fix formatting issues.
- run `make golangci-lint` to find lint issues.
- if the only issues reported come from `typecheck`, fix those first.
  golangci-lint reports only compilation errors for a package it cannot
  build. These errors hide every other finding in the tree. A stale file left
  behind in the working tree is enough to hide what CI will report.

## Four kinds of text

Each kind of text has its own reader and its own job. Put a piece of
information where its reader looks for it, and only there.

- A code comment serves the developer who reads or changes the code. It
  says what the code does, and how and why where the code alone does not
  show it. Algorithms and data structures are explained here.
- A commit message serves the maintainer who finds the commit later with
  `git log` or `git blame`. The subject says what the change does. The
  body says why: the problem, the reason for this solution, and the
  limits that the author knew of. It does not describe how the code
  works or how it is organized. A change whose reason is obvious has no
  body.
- A PR description serves the reviewer before and during the review. It
  gives the context that the diff does not show: the problem, how the
  change is split over files and packages, how it connects to existing
  code and interfaces, where to start reading, and how it was tested. It
  points to the code comments for the details.
- Documentation serves the user who wants to use the software, not to
  change it. It tells what the software does, how to set it up and use
  it, and what to do when it fails. It outlines the design only as far
  as the user needs it.

Code comments and documentation describe only the current state. Git
history has the earlier states.

## Commit messages

- The subject is "area: summary", with a lowercase area. The summary is
  in the imperative mood and names concrete things: a test, a file, a
  flag.
- The body is one or two short paragraphs. Leave out what the code, its
  comments and the PR description show: the parts of a component, the
  layout of files and directories, and how an algorithm works.
- If the material does not give the reason for a change, say what the
  change makes possible. Do not invent a reason or a history.
- A commit message stands alone. It does not refer to other commits.
- Wrap the body at 72 columns. Never shorten names, numbers, paths,
  flags or error messages.
- End every commit with a `Signed-off-by` line. An AI-assisted commit may
  also have an `Assisted-by` line before it, which names the model. Do not
  add a `Co-Authored-By` line for the AI.

  ```
  Assisted-by: Claude Opus 5.5 <noreply@anthropic.com>
  Signed-off-by: Jane Doe <jane.doe@example.com>
  ```

- Ask for "subject only", "caveman length" (the shortest correct body)
  or "explain it" (as long as the reasoning needs) to change the length.

## PR descriptions

- The title names the change.
- Start with what the PR does and why. Do not state its place in a
  series or its base branch, and do not list the commits. GitHub shows
  them.
- End with a Testing section that says how you tested the change.
- Write each paragraph and each list item on one line.
- Do not add a "Generated with ..." line.

## Code

- Add a file, a type or a wrapper only when it carries something new.
- Start a package as one implementation file and its test.
- Put the package comment on the `package` clause. Add a `doc.go` only for
  long package documentation or `go:generate` lines.
- Use a type from the standard library or the repo when one fits.
- Check what a library already supports before you write an adapter for
  it.

## Code comments

- The first line of a function comment is one sentence. It tells what
  the function returns and, if the function has side effects, what it
  does. A simple function needs only this line.
- A complex function may have a few more sentences about what it does.
  It does not say how it works: comments inside the function do that.
- A function comment does not describe its callers, where they call
  from or what they do with the result. The one exception is a lock
  rule on an interface method.
- Document many or complex parameters as a list, one per line.

## Wording

These rules apply to all four kinds of text. Many readers are not native
English speakers. Write plain, natural English that they understand on
the first read: common words, active voice, short sentences, and linking
words such as "because", "so" and "therefore" where they show how
sentences relate. Do not join clauses with semicolons. Say "the driver
image", not "the image of the driver". Documentation has room for
complete, connected paragraphs. Use it.

Make the actor the subject of a sentence: the component, program or
person that does something, such as the driver, the scheduler or the
kernel. A directory, a file or a value is rarely the actor. Put what
the reader already knows first in a sentence and the new information
last. Give each sentence one main fact.

Give a reason in the same sentence as the fact it explains, with
"because" or "so". Prefer a possessive or a compound noun to "the X of
the Y": "the driver state", "the writer's node".

Write from the code or the diff, not from an earlier text. Use only
ASCII characters. Refer only to things a reviewer can see: code, git
history, PRs, issues and public documents. Never refer to local files,
plan documents, task numbers or local branches.

## E2E tests

- Goal is fast test execution and robust tests.
- Avoid timing with "sleep" as in `act; sleep 5; verify`. Instead,
  prefer `act; wait-until-verified`. It runs the first verification
  attempt immediately. Then it repeats the attempt every second until
  success or a timeout, for instance 30 seconds.
