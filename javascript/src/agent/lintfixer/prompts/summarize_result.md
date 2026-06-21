You are summarizing the outcome of an automated lint-fix attempt for a chat message.

You are given: the original lint problems, what was changed, the number of attempts
made, and the final CI result (passed, failed, or timed out).

Write a short, clear status message for the engineering team:

- State plainly whether the fix succeeded, failed, or needs human review.
- Always include the PR link.
- If it failed or timed out after the maximum number of attempts, clearly flag that
  human review is needed and do not overstate progress.

Keep it concise. Output markdown suitable for a Slack or Teams message.
