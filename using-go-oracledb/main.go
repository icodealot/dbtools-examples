// Command using-go-oracledb connects to an Oracle database with the go-oracledb
// thin driver, taking every connection detail (user, password, connect
// descriptor and mTLS wallet) from an OCI Database Tools connection rather than
// from local configuration files.
//
// Set CONNECTION_OCID to the OCID of a Database Tools connection that uses
// PASSWORD authentication and a PEM key store. The caller's OCI principal needs
// read access to that connection and to the Vault secrets it references.
package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/oracle/go-oracledb/v26/oracle"
	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/databasetools"
	"github.com/oracle/oci-go-sdk/v65/secrets"
)

// walletFileName is the file the driver looks for inside the directory named by
// ConnectionProperties.WalletLocation. Don't change this, just included here for
// clarity since the Database Tools service supports many wallet formats.
const walletFileName = "ewallet.pem"

// walletPasswordEnvVar is currently the only way to hand the driver a wallet password
// as of v26.0.0-beta. OracleDriverConfig has no equivalent field, and the driver reads
// this environment variable itself while preparing each connection.
const walletPasswordEnvVar = "ORACLE_GO_WALLET_PASSWORD"

// clientBundle holds the OCI service clients this example needs so they are
// built once and shared across calls.
type clientBundle struct {
	DbtoolsClient databasetools.DatabaseToolsClient
	SecretsClient secrets.SecretsClient
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	id := os.Getenv("CONNECTION_OCID")
	if id == "" {
		return fmt.Errorf("set CONNECTION_OCID to a Database Tools connection OCID, " +
			"for example ocid1.databasetoolsconnection.oc1.<region>.<unique_id>")
	}

	// Set a context timeout within which the demo should complete
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// OCI services publish SDK clients that call REST endpoints so we construct them first
	clients, err := getClientBundle(common.DefaultConfigProvider())
	if err != nil {
		return fmt.Errorf("creating OCI clients: %w", err)
	}

	// !!! This is really the whole purpose of this demo. We construct a go-oracledb OracleDriverConfig
	// !!! from a Database Tools connection. Dig in here!
	oracleDriverConfig, err := getOracleDriverConfigFromConnection(ctx, id, clients)
	if err != nil {
		return fmt.Errorf("building driver config from connection: %w", err)
	}

	// The driver re-reads the wallet from disk every time the pool opens a new
	// connection so the directory has to survive for as long as the pool does.
	defer os.RemoveAll(oracleDriverConfig.ConnectionProperties.WalletLocation)

	// ---------------- Use the go-oracledb driver to connect to Oracle Database ---------------------------------
	connector, err := oracle.NewOracleConnector(oracleDriverConfig)
	if err != nil {
		return fmt.Errorf("creating connector: %w", err)
	}

	db := sql.OpenDB(connector)
	defer db.Close()

	// Ping borrows a connection from the pool and checks for wallet and authentication
	// problems before the first query.
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("validating session: %w", err)
	}

	// ---------------- Send queries to the database!!! ---------------------------------
	var currentUser string
	if err := db.QueryRowContext(ctx, "select user from dual").Scan(&currentUser); err != nil {
		return fmt.Errorf("querying current user: %w", err)
	}

	var versionString string
	query := "select banner_full from v$version"
	if err := db.QueryRowContext(ctx, query).Scan(&versionString); err != nil {
		return fmt.Errorf("querying database version: %w", err)
	}

	fmt.Printf("Connected as user: %s\nDatabase version: %s\n", currentUser, versionString)
	fmt.Printf("DB stats: %+v\n", db.Stats())

	return nil
}

func getClientBundle(config common.ConfigurationProvider) (*clientBundle, error) {
	bundle := clientBundle{}

	dbtoolsClient, err := databasetools.NewDatabaseToolsClientWithConfigurationProvider(config)
	if err != nil {
		return nil, err
	}
	bundle.DbtoolsClient = dbtoolsClient

	secretsClient, err := secrets.NewSecretsClientWithConfigurationProvider(config)
	if err != nil {
		return nil, err
	}
	bundle.SecretsClient = secretsClient

	return &bundle, nil
}

func getOracleDriverConfigFromConnection(ctx context.Context, id string, clients *clientBundle) (*oracle.OracleDriverConfig, error) {
	connection, err := getDatabaseToolsConnection(ctx, id, clients)
	if err != nil {
		return nil, err
	}

	// UserName and UserPassword are optional in the API because TOKEN
	// authentication does not use them but this demo requires PASSWORD
	if connection.AuthenticationType != databasetools.AuthenticationTypePassword {
		return nil, fmt.Errorf("this example expects PASSWORD authentication but the connection uses %s",
			connection.AuthenticationType)
	}

	if connection.UserName == nil {
		return nil, fmt.Errorf("the connection has no user name")
	}

	// Note, if you are using a GENERIC_JDBC Database Tools connection then you should
	// use `connection.Url` isntead.
	if connection.ConnectionString == nil {
		return nil, fmt.Errorf("the connection has no connection string")
	}

	password, err := getConnectionPassword(ctx, connection, clients)
	if err != nil {
		return nil, err
	}

	walletPassword, err := getWalletPassword(ctx, connection, clients)
	if err != nil {
		return nil, err
	}

	if len(walletPassword) > 0 {
		if err := os.Setenv(walletPasswordEnvVar, string(walletPassword)); err != nil {
			return nil, fmt.Errorf("setting %s: %w", walletPasswordEnvVar, err)
		}
	}

	walletDir, err := storeWalletInTempDir(ctx, connection, clients)
	if err != nil {
		return nil, err
	}

	oracleDriverConfig := oracle.NewOracleDriverConfig()
	oracleDriverConfig.Credentials.User = *connection.UserName
	oracleDriverConfig.Credentials.Password = string(password)
	oracleDriverConfig.ConnectDescriptor = *connection.ConnectionString
	oracleDriverConfig.ConnectionProperties.WalletLocation = walletDir

	// Database Tools also supports and returns advancedProperties, however,
	// this example ignores them. It also hardcodes DN matching. A service
	// should consider mapping properties to ConnectionProperties instead.
	oracleDriverConfig.ConnectionProperties.SSLServerDNMatch = true

	return oracleDriverConfig, nil
}

func getDatabaseToolsConnection(ctx context.Context, id string, clients *clientBundle) (*databasetools.DatabaseToolsConnectionOracleDatabase, error) {
	req := databasetools.GetDatabaseToolsConnectionRequest{
		DatabaseToolsConnectionId: common.String(id),
	}

	res, err := clients.DbtoolsClient.GetDatabaseToolsConnection(ctx, req)
	if err != nil {
		return nil, err
	}

	connection, ok := res.DatabaseToolsConnection.(databasetools.DatabaseToolsConnectionOracleDatabase)
	if !ok {
		return nil, fmt.Errorf("connection %s is not an Oracle Database connection", id)
	}

	return &connection, nil
}

// Database Tools connectiosn reference secrets in an OCI Vault by OCID. Secrets are not
// stored by the service. So we need to get passwords and wallets from a vault.
func getConnectionPassword(ctx context.Context, connectionDetails *databasetools.DatabaseToolsConnectionOracleDatabase, clients *clientBundle) ([]byte, error) {
	passwordSecret, ok := connectionDetails.UserPassword.(databasetools.DatabaseToolsUserPasswordSecretId)
	if !ok {
		return nil, fmt.Errorf("unable to get password secret details")
	}

	if passwordSecret.SecretId == nil {
		return nil, fmt.Errorf("the password secret OCID is nil")
	}

	password, err := getSecretBytes(ctx, clients, passwordSecret.SecretId)
	if err != nil {
		return nil, fmt.Errorf("reading password secret: %w", err)
	}

	return password, nil
}

// getWalletPassword returns the password protecting the PEM wallet's private
// key, or nil when the key store has no password configured.
func getWalletPassword(ctx context.Context, connectionDetails *databasetools.DatabaseToolsConnectionOracleDatabase, clients *clientBundle) ([]byte, error) {
	keyStore, err := findPemKeyStore(connectionDetails)
	if err != nil {
		return nil, err
	}

	// A PEM wallet whose private key is not encrypted has no password secret, so
	// an absent password is a valid result though you will find the UI requires
	// a wallet password secret.
	if keyStore.KeyStorePassword == nil {
		return nil, nil
	}

	walletPasswordSecret, ok := keyStore.KeyStorePassword.(databasetools.DatabaseToolsKeyStorePasswordSecretId)
	if !ok {
		return nil, fmt.Errorf("unable to get keystore password secret details")
	}

	if walletPasswordSecret.SecretId == nil {
		return nil, fmt.Errorf("the keystore password secret OCID is nil")
	}

	password, err := getSecretBytes(ctx, clients, walletPasswordSecret.SecretId)
	if err != nil {
		return nil, fmt.Errorf("reading keystore password secret: %w", err)
	}

	return password, nil
}

// storeWalletInTempDir writes the connection's PEM wallet to a temporary directory
// and returns its path. The caller owns the directory and must remove it. On any
// error the directory is cleaned up here and "" is returned.
func storeWalletInTempDir(ctx context.Context, connectionDetails *databasetools.DatabaseToolsConnectionOracleDatabase, clients *clientBundle) (string, error) {
	keyStore, err := findPemKeyStore(connectionDetails)
	if err != nil {
		return "", err
	}

	// A Database Tools connection is not required to have a key store. i.e. if the
	// Oracle Database does not require mTLS then wallets are optional.
	if keyStore == nil {
		return "", nil
	}

	walletSecret, ok := keyStore.KeyStoreContent.(databasetools.DatabaseToolsKeyStoreContentSecretId)
	if !ok {
		return "", fmt.Errorf("unable to get keystore content secret details")
	}

	if walletSecret.SecretId == nil {
		return "", fmt.Errorf("the keystore content secret OCID is nil")
	}

	wallet, err := getSecretBytes(ctx, clients, walletSecret.SecretId)
	if err != nil {
		return "", fmt.Errorf("reading wallet secret: %w", err)
	}

	tempDir, err := os.MkdirTemp("", "wallet-*")
	if err != nil {
		return "", err
	}

	if err := os.WriteFile(filepath.Join(tempDir, walletFileName), wallet, 0o600); err != nil {
		os.RemoveAll(tempDir)
		return "", err
	}

	return tempDir, nil
}

// findPemKeyStore returns the connection's PEM key store. A connection can configure
// one of several key stores. A connection with JKS has separate key store and trust store
// entries, for example.
func findPemKeyStore(connectionDetails *databasetools.DatabaseToolsConnectionOracleDatabase) (*databasetools.DatabaseToolsKeyStore, error) {
	keyStores := connectionDetails.KeyStores
	if keyStores == nil || len(keyStores) == 0 {
		return nil, nil
	}

	for i := range keyStores {
		if keyStores[i].KeyStoreType == databasetools.KeyStoreTypePem {
			return &keyStores[i], nil
		}
	}

	// Database Tools supports PKCS12, SSO and Java key stores as well, but the
	// go-oracledb thin driver reads PEM only.
	return nil, fmt.Errorf("the connection has no PEM key store; go-oracledb requires PEM")
}

// getSecretBytes is a reusable helper that fetches the current bundle for a Vault secret
// and returns its decoded contents.
func getSecretBytes(ctx context.Context, clients *clientBundle, secretID *string) ([]byte, error) {
	req := secrets.GetSecretBundleRequest{
		SecretId: secretID,
	}

	bundle, err := clients.SecretsClient.GetSecretBundle(ctx, req)
	if err != nil {
		return nil, err
	}

	content, ok := bundle.SecretBundleContent.(secrets.Base64SecretBundleContentDetails)
	if !ok {
		return nil, fmt.Errorf("unexpected secret content type %T", bundle.SecretBundleContent)
	}

	if content.Content == nil {
		return nil, fmt.Errorf("the secret bundle has no content")
	}

	return base64.StdEncoding.DecodeString(*content.Content)
}
