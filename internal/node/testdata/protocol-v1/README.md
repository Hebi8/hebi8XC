# Protocol 1, as build 3dc8c5a spoke it

These files are what nodes of build `3dc8c5a` sent each other: the last build
before nodes said which protocol they speak. The data is made up; the shapes
are exact. `protocol_test.go` checks two things against them:

- this build understands every answer here, as a reader;
- this build's own answers still hold every field that is here, with the same
  meaning, so a reader of that build still understands them.

They are not to be edited to make a test pass. A change that needs these files
changed is a new protocol version. See "Protocol compatibility" in
`docs/design.md`.

| File | What |
| --- | --- |
| `hello.request.json`, `hello.response.json` | `POST /v1/hello` |
| `requests.json` | the two `GET` requests of the session module, as that build wrote them |
| `sessions.response.json` | a page of the session feed: one session with every field, one deletion |
| `messages.response.json` | a page of messages: a plain one, and one of a side thread that was cut |
| `messages.response.no-epoch.json` | the same from a build before `epoch` was added to it; still to be understood |
