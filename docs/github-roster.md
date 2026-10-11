# GitHub as the roster: verified facts

Next step: replace the local `trusted_keys` file with the SSH signing keys registered on the user's GitHub account. Each node registers its public key as a signing key once; every node reads the account's signing keys as its roster.

Everything below was checked on 2026-10-11 against the linked GitHub documentation. "Tested" means the call was also made that day, read-only, against github.com. Nothing was written to any account.

## The app

Login goes through a GitHub App, not an OAuth App.

| | |
| --- | --- |
| Owner | the `Hebi8` organization |
| App ID | 5273992 |
| Client ID | `Iv23liTrm1TRc0ZbMP1g` |

The client ID is a public value and will be compiled into `h8c`. According to the owner, Device Flow is enabled and the account permission "SSH signing keys" is set to read and write. Neither setting was checked from here.

## Device flow for a GitHub App

Source: [Generating a user access token for a GitHub App](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-a-user-access-token-for-a-github-app#using-the-device-flow-to-generate-a-user-access-token)

1. `POST https://github.com/login/device/code` with `client_id`. The answer has `device_code`, `user_code`, `verification_uri` (`https://github.com/login/device`), `expires_in` (900 s by default) and `interval` (5 s by default).
2. The user opens the verification URI in any browser and types the `user_code`.
3. The app polls `POST https://github.com/login/oauth/access_token` with `client_id`, `device_code` and `grant_type=urn:ietf:params:oauth:grant-type:device_code`, no faster than `interval`.

- **Only the client ID is needed.** The parameter tables of both requests list no client secret. The one `scope` value accepted is `offline_access`, which asks for an expiring token with a refresh token even if the app has expiry switched off: "No other scopes are supported for GitHub Apps."
- Device flow is off by default: "Before you can use the device flow, you must first enable it in your app's settings." Otherwise polling returns `device_flow_disabled`.
- Polling errors to handle: `authorization_pending` (keep polling), `slow_down` (add 5 s to the interval), `expired_token`, `access_denied`, `incorrect_device_code`, `incorrect_client_credentials`, `unsupported_grant_type`.
- The user access token starts with `ghu_` and expires after 8 hours (`expires_in` is 28800). The refresh token starts with `ghr_` and expires after 6 months (`refresh_token_expires_in` is 15897600).
- Refreshing: `POST https://github.com/login/oauth/access_token` with `client_id`, `grant_type=refresh_token` and `refresh_token`. `client_secret` is "Required unless the user access token was generated using the device flow." After use, "that refresh token and the old user access token will no longer work." Expiry can be switched off per app under Optional Features. Source: [Refreshing user access tokens](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/refreshing-user-access-tokens)

## Permissions of a GitHub App

Source: [Permissions required for GitHub Apps](https://docs.github.com/en/rest/authentication/permissions-required-for-github-apps?apiVersion=2022-11-28#user-permissions-for-ssh-signing-keys) (a long page; the two sections are near the end)

What a GitHub App's token may do is set by permissions, not scopes. A user access token is "limited to the permissions that both your app and the user have."

| Account permission | Access | Endpoints |
| --- | --- | --- |
| SSH signing keys | write | `POST /user/ssh_signing_keys`, `DELETE /user/ssh_signing_keys/{id}` |
| SSH signing keys | read | `GET /user/ssh_signing_keys`, `GET /user/ssh_signing_keys/{id}` |
| Git SSH keys | write | `POST /user/keys`, `DELETE /user/keys/{id}` |
| Git SSH keys | read | `GET /user/keys`, `GET /user/keys/{id}`, `GET /users/{username}/keys` |

- Registering a node key needs "SSH signing keys" with write access. "Git SSH keys" is a separate permission and covers authentication keys; the app must not ask for it.
- All of these accept user access tokens only, except `GET /users/{username}/keys`, which also accepts installation tokens.

## Who can log in

Source: [Making a GitHub App public or private](https://docs.github.com/en/apps/creating-github-apps/registering-a-github-app/making-a-github-app-public-or-private)

- Public: "any user on GitHub can install it and authorize it."
- Private: "it can only be installed on the account that owns the app", and for an app owned by an organization, "Only members of the organization that owns it can authorize it."

While the app is private, only members of `Hebi8` can log in with it.

## SSH signing key endpoints

Source: [REST API endpoints for SSH signing keys](https://docs.github.com/en/rest/users/ssh-signing-keys?apiVersion=2022-11-28)

| Call | Scope, for OAuth App tokens and classic personal access tokens only |
| --- | --- |
| `GET /user/ssh_signing_keys` | `read:ssh_signing_key` |
| `GET /user/ssh_signing_keys/{id}` | `read:ssh_signing_key` |
| `POST /user/ssh_signing_keys` (`key` required, `title` optional) | `write:ssh_signing_key` |
| `DELETE /user/ssh_signing_keys/{id}` | `admin:ssh_signing_key` |
| `GET /users/{username}/ssh_signing_keys` | none |

- A key object has `id`, `key`, `title`, `created_at`.
- The scope column does not apply to the GitHub App above; its tokens are governed by the permissions in the previous section. It matters for `gh`, which logs in as an OAuth App.
- Tested with a `gh` token that lacks these scopes: `GET /user/ssh_signing_keys` returns 404, and the response header `X-Accepted-OAuth-Scopes` lists `admin:ssh_signing_key, read:ssh_signing_key, write:ssh_signing_key`.
- The `*:ssh_signing_key` scopes are named on the REST page but missing from the [scopes table for OAuth Apps](https://docs.github.com/en/apps/oauth-apps/building-oauth-apps/scopes-for-oauth-apps) (checked the same day).
- CLI equivalent for a manual setup: `gh ssh-key add KEY-FILE --type signing` ([Adding a new SSH key](https://docs.github.com/en/authentication/connecting-to-github-with-ssh/adding-a-new-ssh-key-to-your-github-account)).

## Listing a user's signing keys needs no login

Source: [List SSH signing keys for a user](https://docs.github.com/en/rest/users/ssh-signing-keys?apiVersion=2022-11-28#list-ssh-signing-keys-for-a-user): "Lists the SSH signing keys for a user. This operation is accessible by anyone."

- Tested: `GET https://api.github.com/users/{username}/ssh_signing_keys` without credentials returns 200 and a JSON array (empty for an account with no signing keys).
- Unauthenticated requests are limited to 60 per hour per IP address ([Rate limits](https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api?apiVersion=2022-11-28)). Tested: the response carries `x-ratelimit-limit: 60`, an `etag` and `cache-control: public, max-age=60`.
- A 304 answer to `If-None-Match` is free only for authenticated requests ([Best practices](https://docs.github.com/en/rest/using-the-rest-api/best-practices-for-using-the-rest-api?apiVersion=2022-11-28#use-conditional-requests-if-appropriate)). Tested: an unauthenticated 304 used one request of the 60.
- `https://github.com/{username}.keys` and `GET /users/{username}/keys` list authentication keys, not signing keys.

## Signing keys versus authentication keys

Sources: [About SSH](https://docs.github.com/en/authentication/connecting-to-github-with-ssh/about-ssh), [Adding a new SSH key](https://docs.github.com/en/authentication/connecting-to-github-with-ssh/adding-a-new-ssh-key-to-your-github-account), [REST API endpoints for Git SSH keys](https://docs.github.com/en/rest/users/keys?apiVersion=2022-11-28), [About commit signature verification](https://docs.github.com/en/authentication/managing-commit-signature-verification/about-commit-signature-verification)

- **Confirmed: an authentication key grants repository access.** "You can access and write data in repositories on GitHub using SSH." A node key registered as an authentication key could push to every repository the account can write to. Never register node keys there. Those keys use other endpoints (`/user/keys`) and another permission ("Git SSH keys"), so a token of an app that only has "SSH signing keys" cannot add one.
- The two lists are separate: "If you want to use the same SSH key for both authentication and signing, you need to upload it twice."
- "There's no limit on the number of signing keys you can add to your account."
- **Not stated in the documentation:** that a key registered only as a signing key cannot authenticate Git or SSH. The "upload it twice" rule implies it. Checking it means adding a key to the account, so it was not tested. Check it first in the next step: register a signing-only key, then `ssh -i <key> -T git@github.com` must be denied.
- "If you haven't used your SSH key for a year, then GitHub will automatically delete your inactive SSH key." Whether this applies to signing keys, which are never used against GitHub in this design, is not stated.

## What this means for the next step

- Reading the roster needs no token. Only registering a node's key does, with a user access token of the app; that can also be done by hand with `gh`.
- The token lasts 8 hours and is needed once per node, to register the key. Nothing has to be stored unless the node should manage its key later; then it must keep the refresh token and replace it on every refresh.
- A background refresh of the roster should stay far below 60 requests per hour while it runs without a token.
- Anyone who can add a signing key to the account joins the network. The GitHub account, and anything allowed to write its signing keys, becomes part of the trust base.
- The account's signing keys are public, including the ones used for commits. Every signing key on the account is a roster member unless node keys are told apart, for example by title.
