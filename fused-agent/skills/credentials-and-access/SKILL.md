---
name: credentials-and-access
description: Explain Fused buckets, credential references, auth selections and access controls without viewing or creating secrets or changing permissions.
---

# Credentials and access

1. Read the current page before explaining bucket names, selected auth schemes, roles or credential warnings. A missing-credentials error identifies the attempted scheme; it does not prove that other schemes are absent.
2. Distinguish service authentication credentials from ordinary named secrets. Use visible service, bucket and auth scheme names rather than guessing from an ID.
3. Help choose among visible existing references when the user has specified the choice. Do not infer secret values, auth values, or permissions from tool availability.
4. Passwords, tokens, secret values and data-fused-visible=false containers are private. Never request, create, fill or reveal those values. The user enters them in the bucket UI.
5. Draft tools cannot create a secret or save access changes. Navigate to an available bucket/access link and explain the relevant controls without submitting.
6. Skills are guidance, not RBAC grants. Report permission failures only when the tool actually returns one; never enumerate privileges from the list of skills.
