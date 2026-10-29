# Developer Certificate of Origin

Pinvou Agent uses the [Developer Certificate of Origin 1.1](https://developercertificate.org/).

By adding a `Signed-off-by` trailer to a commit, you certify that you have the right to submit the contribution under the license of this repository and that the contribution meets the DCO 1.1 terms.

Create a signed-off commit with:

```bash
git commit -s
```

The trailer should match the commit author:

```text
Signed-off-by: Your Name <your.email@example.com>
```

Every commit in a pull request must be signed off. This is a developer attestation, not a GPG or SSH cryptographic signature.

How CI enforces this (`.github/workflows/dco.yml`): the check verifies that a `Signed-off-by:` trailer is present in each commit's message; it does not compare the trailer's name/email against the commit author — keeping them in sync is the developer's responsibility. Trusted Dependabot and GitHub Actions bot commits, and merge commits (more than one parent), are exempt.

Note that the commit-message gate in `pr-check` (`scripts/validate-commit-msg.py`) is stricter than the DCO workflow above: on pull requests it additionally requires the trailer's signer to match the commit author. A trailer copied from someone else therefore passes the DCO check but still fails `pr-check` — always create sign-offs with `git commit -s` rather than hand-written trailers. Conversely, the DCO workflow is slightly stricter about trailer *shape*: a few hand-written forms (no space after the colon or before the angle bracket, trailing whitespace after the email, an empty name, or an email without `@`) pass `pr-check`'s looser format regex but fail the DCO workflow. `git commit -s` never produces any of these shapes.
