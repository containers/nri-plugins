---
name: coding-style
description: "Use for writing, reviewing or cleaning up code, comments, commit messages and PR descriptions in nri-plugins and its e2e tests. Gives style guidelines."
---

# Code generation and review guidelines

## Automatic checks

- run `make reformat` to find and automatically fix formatting issues.
- run `make golangci-lint` to find lint issues.
- if the only issues reported come from `typecheck`, fix those first.
  golangci-lint reports only compilation errors for a package it cannot
  build. These errors hide every other finding in the tree. A stale file left
  behind in the working tree is enough to hide what CI will report.

## Commit messages

- Subject line is "area: summary" - lowercase area.

- Write the summary in the imperative mood. It completes the sentence
  "If applied, this commit will ...".

- Name concrete things in the summary, such as a test, a file or a flag.
  Do not use terms that only a README or a design document explains.
  - Bad: "e2e: describe the DRA test for a host of its own"
  - Good: "e2e: document how to run test49 on real hardware"

- Say what changed and the one consequence a reader needs. Leave out
  restatement of the diff, rationale that belongs in a comment beside the
  code, and closing summaries.

- Write a body that makes sense without the subject.

- Wrap the body at 72 columns.

- Never shorten names, numbers, paths, flags or error messages.

- End every commit with a `Signed-off-by` line. An AI-assisted commit also
  has an `Assisted-by` line before it. This line names the model:

  ```
  Assisted-by: Claude Opus 5.5 <noreply@anthropic.com>
  Signed-off-by: Jane Doe <jane.doe@example.com>
  ```

  Do not add a `Co-Authored-By` line for the AI.

### Length

The default body is three to five lines. A change that speaks for itself has
no body at all.

Ask for one of these targets when a change wants a different length. They set
the length only. The prose stays plain English either way.

- "caveman length": the shortest body that is still correct, in the spirit of
  the caveman skill. Every sentence carries something the subject line does
  not. Usually one or two lines.
- "subject only": no body.
- "explain it": as long as the change needs. Use it when a reviewer cannot
  get the reasoning from the diff.

## PR descriptions

- Name the specific change in the title.

- Start the description with what the PR does. Do not state its place in a
  series or its base branch. GitHub shows both.

- Do not list the commits.

- End with a Testing section. It says how you tested the change.

- Write each paragraph and each list item on one line. Do not wrap them.

- Do not add a "Generated with ..." line.

## Code

- Add a file, a type or a wrapper only when it carries something new.

- Start a package as one implementation file and its test.

- Put the package comment on the `package` clause. Add a `doc.go` only for
  long package documentation or `go:generate` lines.

- Use a type from the standard library or the repo when one fits. For
  instance, use `sync.Locker` instead of a local `Locker` interface.

- Check what a library already supports before you write an adapter for
  it.

## Code comments

- Always use plain ASCII characters. Also in documentation.

- Document only current state of code and usage. Never document how
  the code worked earlier. Git history has that.

- Function documentation:
  - Document simple functions with a single comment line.
  - The first comment line always tells "what" the function returns.
    For a function with side effects, it also tells what the function
    does. The line is one sentence.
  - In documentation of complex functions, next few sentences may give
    more details on *what* the function does. Avoid documenting what
	the function does not do.
  - Function documentation must not document *how* the function
    works. Comment lines inside the function do that.
  - Function documentation must not include guidelines or assumptions
    about the callers: who calls the function, from where and why.
    For instance, never document where the call comes from. Never
    document how the caller continues after any return value.
  - The one exception is a lock rule on an interface method. Its
    documentation may say that a caller must or must not hold a lock.
  - For a function with many or complex input parameters, document the
    parameters in an itemized list, one per line.

## Wording

These rules apply to code comments, documentation, commit messages and PR
descriptions. Write for readers whose first language is not English.

- Use short sentences and common words. Avoid idioms.

- Use one clause per sentence. Split sentences joined with "and", "but",
  "because", "which" or a comma.
  - Bad: "The driver removes stale specs, which keeps the CDI directory
    clean."
  - Good: "The driver removes stale specs. This keeps the CDI directory
    clean."

- Do not chain "of" phrases. Use a verb or a compound noun instead.
  - Bad: "the form of the tags of main builds"
  - Good: "Main builds have tags like ..."

- Use active voice. Name who does the action.
  - Bad: "Only options which keep the host, the connection, the tasks and
    the variables of the run are taken."
  - Good: "Ansible gets only options that keep the host, the connection and
    the variables."

- Refer only to things a reviewer can see: code, git history, PRs, issues
  and public documents. Never refer to local files, plan documents, task
  numbers or local branches.

## E2E tests

- Goal is fast test execution and robust tests.
- Avoid timing with "sleep" as in `act; sleep 5; verify`. Instead,
  prefer `act; wait-until-verified`. It runs the first verification
  attempt immediately. Then it repeats the attempt every second until
  success or a timeout, for instance 30 seconds.
