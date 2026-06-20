You are planning where to add unit tests in a repository and what conventions to
follow.

You are given a list of source files that need tests, and evidence of the
repository's **existing** test conventions — a list of existing test files and one
example. Use this real evidence to decide, for EACH source file, the correct test
file path and framework that match how THIS repository already organizes tests. Do
not assume a convention from the file extension alone — follow what the existing
tests actually show (directory layout, naming, framework).

Output ONLY a JSON array — no prose, no markdown fences:

[
  {"source": "path/to/Source.ext", "test_path": "where this file's test should live, consistent with the repo", "framework": "the test framework", "notes": "naming / imports / base-class conventions to follow"}
]

Use real, repo-relative paths consistent with the existing tests. If you cannot
determine a placement for a source file, omit it from the array.
