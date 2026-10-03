/*
Copyright The Zabbix Operator Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package jobs

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Environment variables and files a Job container reads its connection from. The Job
// builder sets them; credentials are mounted files so they never appear in the pod spec.
const (
	EnvHost        = "ZBX_DB_HOST"
	EnvPort        = "ZBX_DB_PORT"
	EnvName        = "ZBX_DB_NAME"
	EnvSSLMode     = "ZBX_DB_SSLMODE"
	EnvSSLRootCert = "ZBX_DB_SSLROOTCERT"
	EnvSSLCert     = "ZBX_DB_SSLCERT"
	EnvSSLKey      = "ZBX_DB_SSLKEY"

	// CredentialsDir holds the username and password files.
	CredentialsDir = "/etc/zabbix-db"
	// TLSDir holds the CA certificate and the optional client certificate.
	TLSDir = "/etc/zabbix-db-tls"
)

// credentialsDir is where DBConfigFromEnv reads credentials; tests point it elsewhere.
var credentialsDir = CredentialsDir

// DBConfig holds everything needed to connect.
type DBConfig struct {
	Host, Name, User, Password string
	Port                       int
	SSLMode                    string
	SSLRootCert, SSLCert       string
	SSLKey                     string
}

// DBConfigFromEnv reads the connection from the environment and credential files.
func DBConfigFromEnv() (DBConfig, error) {
	c := DBConfig{
		Host:        os.Getenv(EnvHost),
		Name:        os.Getenv(EnvName),
		SSLMode:     os.Getenv(EnvSSLMode),
		SSLRootCert: os.Getenv(EnvSSLRootCert),
		SSLCert:     os.Getenv(EnvSSLCert),
		SSLKey:      os.Getenv(EnvSSLKey),
		Port:        5432,
	}
	if p := os.Getenv(EnvPort); p != "" {
		port, err := strconv.Atoi(p)
		if err != nil {
			return c, fmt.Errorf("%s: %w", EnvPort, err)
		}
		c.Port = port
	}
	user, err := os.ReadFile(credentialsDir + "/username")
	if err != nil {
		return c, err
	}
	pass, err := os.ReadFile(credentialsDir + "/password")
	if err != nil {
		return c, err
	}
	c.User = strings.TrimRight(string(user), "\r\n")
	c.Password = strings.TrimRight(string(pass), "\r\n")
	if c.Host == "" || c.Name == "" {
		return c, fmt.Errorf("%s and %s are required", EnvHost, EnvName)
	}
	return c, nil
}

// connString renders the non-secret parts as a libpq keyword/value string. Values are
// quoted, so no character in a host or path can break parsing.
func (c DBConfig) connString() string {
	kv := []string{
		"host=" + quote(c.Host),
		"port=" + strconv.Itoa(c.Port),
		"dbname=" + quote(c.Name),
		"user=" + quote(c.User),
		"connect_timeout=10",
		"application_name=zabbix-operator",
	}
	mode := c.SSLMode
	if mode == "" {
		mode = "prefer"
	}
	kv = append(kv, "sslmode="+quote(mode))
	for k, v := range map[string]string{"sslrootcert": c.SSLRootCert, "sslcert": c.SSLCert, "sslkey": c.SSLKey} {
		if v != "" {
			kv = append(kv, k+"="+quote(v))
		}
	}
	return strings.Join(kv, " ")
}

func quote(v string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, "'", `\'`).Replace(v) + "'"
}

// Connect opens a connection. The password is set on the parsed configuration and never
// passes through the connection string.
func Connect(ctx context.Context, c DBConfig) (*pgx.Conn, error) {
	cfg, err := pgx.ParseConfig(c.connString())
	if err != nil {
		return nil, err
	}
	cfg.Password = c.Password
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return pgx.ConnectConfig(ctx, cfg)
}
