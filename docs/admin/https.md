# HTTPS

Airspace can get its own certificate from an ACME server and serve HTTPS.
Set `ACME_DOMAINS` to turn it on. It is off by default.

| Setting | Default | Meaning |
|---|---|---|
| `ACME_DOMAINS` | unset | Hostnames to get certificates for, comma separated. Setting it turns TLS on. |
| `ACME_DIRECTORY_URL` | Let's Encrypt | The ACME directory |
| `ACME_EMAIL` | unset | Contact address for the ACME account |
| `ACME_CA_FILE` | unset | PEM root to trust when the ACME server itself has a private certificate |
| `ACME_EAB_KID`, `ACME_EAB_HMAC_KEY` | unset | External account binding, if the server requires it |
| `ACME_CACHE_DIR` | `acme/` beside the database | Where the account key and certificates are kept. Keep it private. |
| `ACME_RENEW_BEFORE` | `auto` | Renew this long before expiry. `auto` is a third of the certificate's life: 30 days for a 90 day certificate, 56 hours for a 7 day one. |
| `TLS_LISTEN_ADDR` | `:443` | HTTPS listen address |

With TLS on, `LISTEN_ADDR` defaults to `:80`. That listener answers only the
ACME HTTP challenge and redirects everything else to HTTPS. If it cannot be
opened, Airspace logs a warning and carries on with HTTPS alone.

Airspace asks for the certificate when it starts and logs the result. It
proves it holds the name with the TLS-ALPN-01 challenge, or HTTP-01. For
either, **the ACME server must be able to connect to Airspace on port 443 or
port 80**, at the address the hostname resolves to.

- **Let's Encrypt** (the default): the hostname must resolve publicly and
  port 443 or 80 must reach this machine from the internet. A host that is
  only on a private network cannot get a Let's Encrypt certificate this way;
  that needs the DNS-01 challenge, which Airspace does not do.
- **A private ACME server**, for example HashiCorp Vault's PKI engine: point
  `ACME_DIRECTORY_URL` at it. The server must resolve the hostname and reach
  the machine. Browsers must trust that server's root certificate.

  ```sh
  ACME_DOMAINS=airspace.example.internal \
  ACME_DIRECTORY_URL=https://vault.example.internal:8200/v1/pki/roles/lan/acme/directory \
  ./bin/airspace
  ```

Wildcard names are not supported. On Linux, a service that is not root needs
`CAP_NET_BIND_SERVICE` to open ports 80 and 443; the systemd unit in
`packaging/systemd/` shows the lines.
