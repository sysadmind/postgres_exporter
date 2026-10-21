// Copyright The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package rdsiam

import (
	"context"
	"database/sql/driver"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/rds/auth"
	"github.com/lib/pq"
)

func TestOpenConnectorRejects(t *testing.T) {
	for name, dsn := range map[string]string{
		"password":      "postgresql://user:secret@db.example.com/postgres",
		"multiple host": "host=a.example.com,b.example.com user=user dbname=postgres",
		"sslmode":       "postgresql://user@db.example.com/postgres?sslmode=prefer",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := (&Driver{}).OpenConnector(dsn); err == nil {
				t.Fatalf("OpenConnector(%q) succeeded, want error", dsn)
			}
		})
	}
}

func TestConnectMintsTokenForEachConnection(t *testing.T) {
	origLoad, origBuild, origConnect := loadAWSConfig, buildToken, connect
	t.Cleanup(func() { loadAWSConfig, buildToken, connect = origLoad, origBuild, origConnect })
	var loads int
	loadAWSConfig = func(_ context.Context, region, roleARN string) (aws.Config, error) {
		loads++
		if region != "us-east-1" || roleARN != "arn:aws:iam::123456789012:role/exporter" {
			t.Errorf("loadAWSConfig(%q, %q), want the driver's region and role", region, roleARN)
		}
		return aws.Config{Region: region}, nil
	}
	var calls int
	buildToken = func(_ context.Context, endpoint, region, user string, _ aws.CredentialsProvider, _ ...func(*auth.BuildAuthTokenOptions)) (string, error) {
		calls++
		if endpoint != "db.example.com:6543" || region != "us-east-1" || user != "exporter" {
			t.Errorf("buildToken(%q, %q, %q), want db.example.com:6543, us-east-1, exporter", endpoint, region, user)
		}
		return "token", nil
	}
	var got pq.Config
	connect = func(_ context.Context, cfg pq.Config) (driver.Conn, error) {
		got = cfg
		return nil, errors.New("not connecting in tests")
	}

	// Discovery rewrites the database; the token only depends on host, port
	// and user, so both DSN forms work unchanged.
	d := &Driver{region: "us-east-1", roleARN: "arn:aws:iam::123456789012:role/exporter"}
	for _, dsn := range []string{
		"postgresql://exporter@db.example.com:6543/postgres",
		"host=db.example.com port=6543 user=exporter dbname=other",
	} {
		c, err := d.OpenConnector(dsn)
		if err != nil {
			t.Fatalf("OpenConnector(%q): %v", dsn, err)
		}
		_, _ = c.Connect(context.Background())
		if got.Password != "token" {
			t.Errorf("connected with password %q, want token", got.Password)
		}
	}
	if calls != 2 {
		t.Errorf("built %d tokens, want 2", calls)
	}
	if loads != 1 {
		t.Errorf("loaded AWS config %d times, want once per driver", loads)
	}
}

func TestDriverNamePerRegionAndRole(t *testing.T) {
	a := DriverName("us-east-1", "arn:aws:iam::123456789012:role/a")
	if again := DriverName("us-east-1", "arn:aws:iam::123456789012:role/a"); again != a {
		t.Errorf("DriverName returned %q then %q for the same region and role", a, again)
	}
	if b := DriverName("us-east-1", "arn:aws:iam::123456789012:role/b"); b == a {
		t.Errorf("DriverName returned %q for two different roles", a)
	}
}
