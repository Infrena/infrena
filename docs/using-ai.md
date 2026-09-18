# Using AI to contribute

**You can use AI tools to work on Infrena.** Assistants, agents, autocomplete, whatever helps.
There is no disclosure requirement and no separate review queue. A good patch is a good patch.

What follows is not a policy about AI. It is a reminder of what contributing already means, in
the places where these tools make it easy to forget.

## You are the author

The person who opens the pull request is the author of every line in it. Not the tool. If a
change breaks something, the conversation is with you, and "the model wrote it" is not an
answer anyone can do anything with.

In practice that means one thing: **submit only what you understand.** You should be able to
say why a change works, what it does when its inputs are wrong, and what else it touches. If
you cannot, the patch is not ready — not because a tool was involved, but because nobody has
understood it yet.

This is the same bar as always. It is worth stating because the cost of producing plausible
code has dropped much faster than the cost of reviewing it, and the gap is where bugs live.

## You are certifying something specific

The [contributor agreement](../CLA.md) asks you to represent that your contribution is your own
original work, and to disclose any third-party licence or restriction you are personally aware
of. That does not change because a tool helped you write it.

If you know a chunk of your patch was reproduced from somewhere with its own licence, say so in
the pull request. Nobody expects you to audit a model's training data — "personally aware" means
what it says.

## Review is about the code

Reviewers read the diff, not its provenance. A change is not weaker because a tool produced it,
and not stronger because a person typed it by hand. The questions are the ones they always were:
is it correct, is it understandable in six months, does it earn its complexity.

If you are reviewing, the same rule applies to you. Approving something you have not understood
is the same mistake in either direction.

## One thing this project has learned the hard way

**A green test suite is not evidence.** Nearly every serious bug in Infrena's first weeks was
found by running the thing against something real — a live S3 bucket, a real archive, the actual
binary on a real closed pipe — and not one was caught by the suite going green.

That matters more, not less, when a tool wrote the tests. It is very easy to generate a test
that asserts what the code does rather than what it should do, and such a test passes forever
while proving nothing. The habit that catches it is cheap: **break the thing on purpose and
watch the test fail.** If it does not fail, it was never testing that.

Run the suite. Then go and check the claim some other way.

## Practical notes

- **Do not paste credentials, tokens or customer data into a tool.** Same rule as pasting them
  into a chat window or an issue.
- **Commit messages are for humans.** Say what changed and why. No need to mention how it was
  written.
- **Generated files stay generated.** If something comes from a generator, change the generator.
