# GitHub as the roster: verified facts

Next step: replace the local `trusted_keys` file with the SSH signing keys registered on the user's GitHub account. Each node registers its public key as a signing key once; every node reads the account's signing keys as its roster.

Everything below was checked on 2026-10-11 against the linked GitHub documentation. "Tested" means the call was also made that day, read-only, against github.com. Nothing was written to any account.

## Device flow for an OAuth App

Source: [Authorizing OAuth apps, Device flow](https://docs.github.com/en/apps/oauth-apps/building-oauth-apps/authorizing-oauth-apps#device-flow)

1. `POST https://github.com/login/device/code` with `client_id` and `scope`. The answer has `device_code`, `user_code`, `verification_uri` (`https://github.com/login/device`), `expires_in` (900 s by default) and `interval` (5 s by default).
2. The user opens the verification URI in any browser and types the `user_code`.
3. The app polls `POST https://github.com/login/oauth/access_token` with `client_id`, `device_code` and `grant_type=urn:ietf:params:oauth:grant-type:device_code`, no faster than `interval`, until it gets an `access_token`.

- **No client secret.** The documentation says: "The `client_secret` is not needed for the device flow." The client id is public and can be compiled into `h8c`.
- Device flow is off by default: "you must first enable it in your app's settings". Otherwise polling returns `device_flow_disabled`.
- Polling errors to handle: `authorization_pending` (keep polling), `slow_down` (add 5 s to the interval), `expired_token`, `access_denied`, `incorrect_device_code`, `incorrect_client_credentials`, `unsupported_grant_type`.

## SSH signing key endpoints

Source: [REST API endpoints for SSH signing keys](https://docs.github.com/en/rest/users/ssh-signing-keys?apiVersion=2022-11-28)

| Call | Scope for OAuth app tokens and classic PATs |
| --- | --- |
| `GET /user/ssh_signing_keys` | `read:ssh_signing_key` |
| `GET /user/ssh_signing_keys/{id}` | `read:ssh_signing_key` |
| `POST /user/ssh_signing_keys` (`key` required, `title` optional) | `write:ssh_signing_key` |
| `DELETE /user/ssh_signing_keys/{id}` | `admin:ssh_signing_key` |
| `GET /users/{username}/ssh_signing_keys` | none |

- A key object has `id`, `key`, `title`, `created_at`.
- Tested: `GET /user/ssh_signing_keys` with a token that lacks these scopes returns 404, and the response header `X-Accepted-OAuth-Scopes` lists `admin:ssh_signing_key, read:ssh_signing_key, write:ssh_signing_key`. So `write` is enough to list as well as create.
- The `*:ssh_signing_key` scopes are named on the REST page but missing from the [scopes table](https://docs.github.com/en/apps/oauth-apps/building-oauth-apps/scopes-for-oauth-apps) (checked the same day). The REST page and the API header agree, so they are the reference.
- CLI equivalent for a manual setup: `gh ssh-key add KEY-FILE --type signing` ([Adding a new SSH key](https://docs.github.com/en/authentication/connecting-to-github-with-ssh/adding-a-new-ssh-key-to-your-github-account)).

## Listing a user's signing keys needs no login

Source: [List SSH signing keys for a user](https://docs.github.com/en/rest/users/ssh-signing-keys?apiVersion=2022-11-28#list-ssh-signing-keys-for-a-user): "Lists the SSH signing keys for a user. This operation is accessible by anyone."

- Tested: `GET https://api.github.com/users/{username}/ssh_signing_keys` without credentials returns 200 and a JSON array (empty for an account with no signing keys).
- Unauthenticated requests are limited to 60 per hour per IP address ([Rate limits](https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api?apiVersion=2022-11-28)). Tested: the response carries `x-ratelimit-limit: 60`, an `etag` and `cache-control: public, max-age=60`.
- A 304 answer to `If-None-Match` is free only for authenticated requests ([Best practices](https://docs.github.com/en/rest/using-the-rest-api/best-practices-for-using-the-rest-api?apiVersion=2022-11-28#use-conditional-requests-if-appropriate)). Tested: an unauthenticated 304 used one request of the 60.
- `https://github.com/{username}.keys` and `GET /users/{username}/keys` list authentication keys, not signing keys.

## Signing keys versus authentication keys

Sources: [About SSH](https://docs.github.com/en/authentication/connecting-to-github-with-ssh/about-ssh), [Adding a new SSH key](https://docs.github.com/en/authentication/connecting-to-github-with-ssh/adding-a-new-ssh-key-to-your-github-account), [REST API endpoints for Git SSH keys](https://docs.github.com/en/rest/users/keys?apiVersion=2022-11-28), [About commit signature verification](https://docs.github.com/en/authentication/managing-commit-signature-verification/about-commit-signature-verification)

- **Confirmed: an authentication key grants repository access.** "You can access and write data in repositories on GitHub using SSH." A node key registered as an authentication key could push to every repository the account can write to. Never register node keys there. Those keys use other endpoints (`/user/keys`) and other scopes (`*:public_key`), so a token with only `write:ssh_signing_key` cannot add one.
- The two lists are separate: "If you want to use the same SSH key for both authentication and signing, you need to upload it twice."
- "There's no limit on the number of signing keys you can add to your account."
- **Not stated in the documentation:** that a key registered only as a signing key cannot authenticate Git or SSH. The "upload it twice" rule implies it. Checking it means adding a key to the account, so it was not tested. Check it first in the next step: register a signing-only key, then `ssh -i <key> -T git@github.com` must be denied.
- "If you haven't used your SSH key for a year, then GitHub will automatically delete your inactive SSH key." Whether this applies to signing keys, which are never used against GitHub in this design, is not stated.

## What this means for the next step

- Reading the roster needs no token. Only registering a node's key does, with scope `write:ssh_signing_key`; that can also be done by hand with `gh`.
- A background refresh should use a token when one is present and stay far below 60 requests per hour when not.
- Anyone who can add a signing key to the account joins the network. The GitHub account, and any app holding `write:ssh_signing_key`, becomes part of the trust base.
- The account's signing keys are public, including the ones used for commits. Every signing key on the account is a roster member unless node keys are told apart, for example by title.
