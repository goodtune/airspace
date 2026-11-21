# How it works

## How it uses OpenSky

- One poller, shared by every browser. Five browsers cost the same as one.
- **Nobody watching, nothing requested.** With no active browser, Airspace
  makes no request to OpenSky at all: no poll, no token request, no
  keep-alive. It makes no request at startup.
- **Viewport first.** Each poll asks only for the area being looked at. A
  whole-world query is an explicit, temporary choice ("Global" coverage).
- A browser is *active* when its connection is open and its tab was visible
  within `HIDDEN_TIMEOUT`. A browser in replay is not active.
- After `IDLE_TIMEOUT` with no input from anyone, polling pauses until
  someone clicks.

## Design notes

- **Hidden tabs.** By default the Datastar client closes its stream when a
  tab is hidden and reopens it when the tab shows again. Airspace opens the
  stream with Datastar's `openWhenHidden` option, so the connection stays up
  and the page reports its visibility. That lets the server apply
  `HIDDEN_TIMEOUT` exactly, and a brief tab switch does not look like a
  disconnect.
- **Aircraft data does not travel over Datastar.** Signals carry control
  state. When the snapshot version changes, each browser fetches
  `/api/states`.
- **No authentication.** Airspace assumes a trusted network. HTTPS encrypts
  the connection; it does not restrict who may connect.
