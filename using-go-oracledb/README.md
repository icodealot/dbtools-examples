# Connecting to Oracle Database with go-oracledb and OCI Database Tools

> Note: This README was drafted with an LLM and edited by me. There is a lot of boilerplate.
  The Go example was primarily coded by me with an LLM refactoring once I had the example
  up and running. The LLM also provided debugging and moral support along the way.

This is a minimal Go example that pulls all connection details (database user,
password, connect descriptor and mTLS wallet) from an OCI Database Tools
connection. These are handed to the go-oracledb thin driver to connect to a database.

_Nothing is read from a local `tnsnames.ora`, wallet directory or `.env` file._

This is example code written to accompany a blog post. It favours being readable
in one sitting over being production-ready. The idea is that you may have a service
written in Go to read an OCID injected at runtime, to configure a connection pool.

If you are unfamiliar with OCI Database Tools, you can learn more about it here:

- [Database Tools Connections](https://icodealot.com/posts/database-tools-connections/)
- [Create Database Tools Connections with Terraform](https://icodealot.com/posts/create-database-tools-connection-with-terraform/)
- [Setup OCI DBTools Connections with an ADB Access Control List](https://icodealot.com/posts/dbtools-connections-with-adb-access-control-list/)
- [OCI Database Tools Documentation](https://docs.oracle.com/en-us/iaas/database-tools/home.htm)

## Requirements

- Go 1.27 or later
- An OCI Database Tools connection for an Oracle database, configured with:
  - **PASSWORD** authentication (not TOKEN)
  - a **PEM** key store — the thin driver cannot read PKCS12, SSO or JKS wallets
- An OCI CLI/SDK configuration at `~/.oci/config`, or the equivalent
  `OCI_CLI_*` environment variables. The example uses
  `common.DefaultConfigProvider()`.

## IAM policies

The OCI principal running the example needs to read the connection and the Vault
secrets it points at:

```
allow group <group> to read database-tools-family in compartment <connection-compartment>
allow group <group> to read secret-bundles in compartment <vault-compartment>
```

`secret-bundles` is the resource type behind the Secret Retrieval API; reading
the secret *metadata* is not enough.

## Running it

```sh
export CONNECTION_OCID=ocid1.databasetoolsconnection.oc1.<region>.<unique_id>
go run .
```

Expected output:

```
Connected as user: YOUR_DB_USER
Database version: ...your db version...
DB stats: {MaxOpenConnections:0 OpenConnections:1 InUse:0 Idle:1 ...}
```

On failure the program writes to stderr and exits non-zero.

## What it does

1. `GetDatabaseToolsConnection` fetches the connection definition.
2. The user password and the wallet both arrive as **OCID references to Vault
   secrets**, not inline, so each secret is retrieved with `GetSecretBundle` and
   base64-decoded.
3. The wallet is written as `ewallet.pem` into a `0700` temporary directory with
   `0600` permissions, and `ConnectionProperties.WalletLocation` is pointed at
   that directory.
4. `oracle.NewOracleConnector` plus `sql.OpenDB` yield an ordinary
   `*sql.DB`.

## Things worth knowing

**The wallet directory must outlive the pool.** The driver re-reads
`ewallet.pem` from disk each time `database/sql` opens a *new* pooled
connection, not only the first. Removing the directory after the first
successful query breaks every subsequent connection. Here the cleanup is
deferred for the lifetime of the process.

**An encrypted wallet password can only be passed via the environment.** As of
`go-oracledb v26.0.0-beta`, `OracleDriverConfig` had no wallet-password field.
The driver reads `oracle.go.wallet_password` or `ORACLE_GO_WALLET_PASSWORD` from
the process environment while preparing each connection, so `getWalletPassword`
fetches the secret and the example sets `ORACLE_GO_WALLET_PASSWORD` before
connecting.

**Connection advanced properties are ignored.** A Database Tools connection supports
`AdvancedProperties`. This example ignores them and hard codes `SSLServerDNMatch = true`,
which is a safe default; a real service should map relevant advanced properties
onto `ConnectionProperties`.

