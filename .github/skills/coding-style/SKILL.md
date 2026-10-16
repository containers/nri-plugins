---
name: coding-style
description: "Use for generating code and comments to nri-plugins and to e2e tests, or to review and clean up existing code. Gives style guidelines."
---

# Code generation and review guidelines

## Automatic checks

- run `make reformat` to find and automatically fix formatting issues.
- run `make golangci-lint` to find lint issues.
- if the only issues reported come from `typecheck`, fix those first: when
  golangci-lint cannot build a package, it reports nothing but the compilation
  errors, hiding every other finding in the tree. A stale file left behind in
  the working tree is enough to hide what CI will report.

## Commit messages

- Subject line is "area: summary." - lowercase area, trailing period.

- Say what changed, and the one consequence a reader needs. Leave out
  restatement of the diff, rationale that belongs in a comment beside the
  code, and closing summaries.

- Names, numbers, paths, flags and error messages are never shortened.

### Length

The default body is three to five lines, and no body at all for a change that
speaks for itself.

Ask for one of these targets when a change wants a different length. They set
the length only, never the language: the prose stays plain English either way.

- "caveman length": the shortest body that is still correct, in the spirit of
  the caveman skill. Every sentence carries something the subject line does
  not. Usually one or two lines.
- "subject only": no body.
- "explain it": as long as the change needs, for one whose reasoning a
  reviewer cannot get from the diff.

## Code comments

- Always use plain ASCII characters. Also in documentation.

- Document only current state of code and usage. Never document how
  something was done earlier because that is in git history.

- Function documentation:
  - Document simple functions with a single comment line.
  - The first comment line always tells "what" the function returns,
    and in case of side effects, what the function does, summarized in
    one sentence.
  - In documentation of complex functions, next few sentences may give
    more details on *what* the function does. Avoid documenting what
	the function does not do.
  - Function documentation must not document *how* the function
    works. That is left to comment lines inside the function.
  - Function documentation must not include guidelines or assumption on
    why, where, or by whom the function is called. For instance, never
    document from where the call is expected to come from or how the
    caller is expected to continue in case of any return value.
  - Functions with many or complex input parameters should have parameters
    documented in an itemized list, one per line.

## E2E tests

- Goal is fast test execution and robust tests.
- Avoid timing with "sleep" as in `act; sleep 5; verify`. Instead,
  prefer `act; wait-until-verified` that runs first verification attempt
  immediately, and keeps repeating it in 1 second interval until
  success or 30 second timeout is reached, for instance.
