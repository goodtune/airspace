# Getting started

This page takes you from nothing to aircraft on the globe.

## Run it

1. Create an OpenSky API client. The next section shows how.
2. Get a binary. Download one from the
   [releases page](https://github.com/goodtune/airspace/releases), or build
   one: `make local` makes `bin/airspace` for the machine you are on. See
   [Installation](admin/installation.md) for packages and containers.
3. Run it:

   ```sh
   OPENSKY_CLIENT_ID=... OPENSKY_CLIENT_SECRET=... ./bin/airspace
   ```

4. Open `http://<host>:8080`.

Every setting is in [Configuration](admin/configuration.md).

## Create an OpenSky API client

1. Register a free account at <https://opensky-network.org> and sign in.
2. Open your **Account** page and find the **API Client** section.
3. Create a client. OpenSky gives you a `client_id` and a `client_secret`.
4. Give them to Airspace as `OPENSKY_CLIENT_ID` and `OPENSKY_CLIENT_SECRET`.

The secret stays on the server. Airspace never sends it, or the access token,
to a browser, and never writes either to the log.

## Credentials from dotvault

If you keep secrets in [dotvault](https://github.com/goodtune/dotvault),
Airspace can read the two values from there. Store a secret with the fields
`clientId` and `clientSecret`, then name it:

```sh
./bin/airspace --dotvault opensky      # or OPENSKY_DOTVAULT=opensky
```

- Do not also set `OPENSKY_CLIENT_ID` or `OPENSKY_CLIENT_SECRET`; Airspace
  refuses to start with both, so there is no doubt which is in use.
- Airspace uses the login dotvault already has for the user it runs as. It
  never opens a browser or prompts. If there is no usable login, it says so
  and stops: run `dotvault login` first.
- It reads dotvault's system configuration file, or
  `~/.config/dotvault/config.yaml` when there is no system file. Set
  `DOTVAULT_CONFIG` (or `--dotvault-config`) for any other location.
- The credentials are read once, at startup. Restart Airspace after you
  change them. This talks to Vault, not to OpenSky, so it does not break the
  rule that startup makes no OpenSky request.
