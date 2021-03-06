// Copyright 2015 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package cli

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pkg/errors"
	"golang.org/x/net/context"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/build"
	"github.com/cockroachdb/cockroach/pkg/security"
	"github.com/cockroachdb/cockroach/pkg/security/securitytest"
	"github.com/cockroachdb/cockroach/pkg/server"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
)

type cliTest struct {
	*server.TestServer
	certsDir    string
	cleanupFunc func() error

	// t is the testing.T instance used for this test.
	// Example_xxx tests may have this set to nil.
	t *testing.T
	// logScope binds the lifetime of the log files to this test, when t
	// is not nil
	logScope *log.TestLogScope
}

type cliTestParams struct {
	t          *testing.T
	insecure   bool
	noServer   bool
	storeSpecs []base.StoreSpec
}

func (c *cliTest) fail(err interface{}) {
	if c.t != nil {
		defer c.logScope.Close(c.t)
		c.t.Fatal(err)
	} else {
		panic(err)
	}
}

func newCLITest(params cliTestParams) cliTest {
	c := cliTest{t: params.t}

	certsDir, err := ioutil.TempDir("", "cli-test")
	if err != nil {
		c.fail(err)
	}
	c.certsDir = certsDir

	if c.t != nil {
		c.logScope = log.Scope(c.t)
	}

	c.cleanupFunc = func() error { return nil }

	if !params.noServer {
		if !params.insecure {
			// Copy these assets to disk from embedded strings, so this test can
			// run from a standalone binary.
			// Disable embedded certs, or the security library will try to load
			// our real files as embedded assets.
			security.ResetAssetLoader()

			assets := []string{
				filepath.Join(security.EmbeddedCertsDir, security.EmbeddedCACert),
				filepath.Join(security.EmbeddedCertsDir, security.EmbeddedCAKey),
				filepath.Join(security.EmbeddedCertsDir, security.EmbeddedNodeCert),
				filepath.Join(security.EmbeddedCertsDir, security.EmbeddedNodeKey),
				filepath.Join(security.EmbeddedCertsDir, security.EmbeddedRootCert),
				filepath.Join(security.EmbeddedCertsDir, security.EmbeddedRootKey),
			}

			for _, a := range assets {
				securitytest.RestrictedCopy(nil, a, certsDir, filepath.Base(a))
			}
			baseCfg.SSLCertsDir = certsDir

			c.cleanupFunc = func() error {
				security.SetAssetLoader(securitytest.EmbeddedAssets)
				return os.RemoveAll(c.certsDir)
			}
		}

		s, err := serverutils.StartServerRaw(base.TestServerArgs{
			Insecure:    params.insecure,
			SSLCertsDir: c.certsDir,
			StoreSpecs:  params.storeSpecs,
		})
		if err != nil {
			c.fail(err)
		}
		c.TestServer = s.(*server.TestServer)

		log.Infof(context.TODO(), "server started at %s", c.ServingAddr())
	}

	baseCfg.User = security.NodeUser

	// Ensure that CLI error messages and anything meant for the
	// original stderr is redirected to stdout, where it can be
	// captured.
	stderr = os.Stdout

	return c
}

// stopServer stops the test server.
func (c *cliTest) stopServer() {
	if c.TestServer != nil {
		log.Infof(context.TODO(), "stopping server at %s", c.ServingAddr())
		select {
		case <-c.Stopper().ShouldStop():
			// If ShouldStop() doesn't block, that means someone has already
			// called Stop(). We just need to wait.
			<-c.Stopper().IsStopped()
		default:
			c.Stopper().Stop(context.TODO())
		}
	}
}

// restartServer stops and restarts the test server. The ServingAddr() may
// have changed after this method returns.
func (c *cliTest) restartServer(params cliTestParams) {
	c.stopServer()
	log.Info(context.TODO(), "restarting server")
	s, err := serverutils.StartServerRaw(base.TestServerArgs{
		Insecure:    params.insecure,
		SSLCertsDir: c.certsDir,
		StoreSpecs:  params.storeSpecs,
	})
	if err != nil {
		c.fail(err)
	}
	c.TestServer = s.(*server.TestServer)
	log.Infof(context.TODO(), "restarted server at %s", c.ServingAddr())
}

// cleanup cleans up after the test, stopping the server if necessary.
// The log files are removed if the test has succeeded.
func (c *cliTest) cleanup() {
	if c.t != nil {
		defer c.logScope.Close(c.t)
	}

	// Restore stderr.
	stderr = log.OrigStderr

	log.Info(context.TODO(), "stopping server and cleaning up CLI test")

	c.stopServer()

	if err := c.cleanupFunc(); err != nil {
		panic(err)
	}
}

func (c cliTest) Run(line string) {
	a := strings.Fields(line)
	c.RunWithArgs(a)
}

// RunWithCapture runs c and returns a string containing the output of c
// and any error that may have occurred capturing the output. We do not propagate
// errors in executing c, because those will be caught when the test verifies
// the output of c.
func (c cliTest) RunWithCapture(line string) (out string, err error) {
	return captureOutput(func() {
		c.Run(line)
	})
}

func (c cliTest) RunWithCaptureArgs(args []string) (string, error) {
	return captureOutput(func() {
		c.RunWithArgs(args)
	})
}

// captureOutput runs f and returns a string containing the output and any
// error that may have occurred capturing the output.
func captureOutput(f func()) (out string, err error) {
	// Heavily inspired by Go's testing/example.go:runExample().

	// Funnel stdout into a pipe.
	stdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		return "", err
	}
	os.Stdout = w

	// Send all bytes from piped stdout through the output channel.
	type captureResult struct {
		out string
		err error
	}
	outC := make(chan captureResult)
	go func() {
		var buf bytes.Buffer
		_, err := io.Copy(&buf, r)
		r.Close()
		outC <- captureResult{buf.String(), err}
	}()

	// Clean up and record output in separate function to handle panics.
	defer func() {
		// Close pipe and restore normal stdout.
		w.Close()
		os.Stdout = stdout
		outResult := <-outC
		out, err = outResult.out, outResult.err
		if x := recover(); x != nil {
			err = errors.Errorf("panic: %v", x)
		}
	}()

	// Run the command. The output will be returned in the defer block.
	f()
	return
}

func (c cliTest) RunWithArgs(origArgs []string) {
	TestingReset()

	if err := func() error {
		args := append([]string(nil), origArgs[:1]...)
		if c.TestServer != nil {
			h, p, err := net.SplitHostPort(c.ServingAddr())
			if err != nil {
				return err
			}
			if c.Cfg.Insecure {
				args = append(args, "--insecure")
			} else {
				args = append(args, "--insecure=false")
				args = append(args, fmt.Sprintf("--certs-dir=%s", c.certsDir))
			}
			args = append(args, fmt.Sprintf("--host=%s", h))
			args = append(args, fmt.Sprintf("--port=%s", p))
		}
		args = append(args, origArgs[1:]...)

		fmt.Fprintf(os.Stderr, "%s\n", args)
		fmt.Println(strings.Join(origArgs, " "))

		return Run(args)
	}(); err != nil {
		fmt.Println(err)
	}
}

func (c cliTest) RunWithCAArgs(origArgs []string) {
	TestingReset()

	if err := func() error {
		args := append([]string(nil), origArgs[:1]...)
		if c.TestServer != nil {
			args = append(args, fmt.Sprintf("--ca-key=%s", filepath.Join(c.certsDir, security.EmbeddedCAKey)))
			args = append(args, fmt.Sprintf("--certs-dir=%s", c.certsDir))
		}
		args = append(args, origArgs[1:]...)

		fmt.Fprintf(os.Stderr, "%s\n", args)
		fmt.Println(strings.Join(origArgs, " "))

		return Run(args)
	}(); err != nil {
		fmt.Println(err)
	}
}

func TestQuit(t *testing.T) {
	defer leaktest.AfterTest(t)()

	if testing.Short() {
		t.Skip("short flag")
	}

	c := newCLITest(cliTestParams{t: t})
	defer c.cleanup()

	c.Run("quit")
	// Wait until this async command cleanups the server.
	<-c.Stopper().IsStopped()

	// NB: if this test is ever flaky due to port reuse, we could run against
	// :0 (which however changes some of the errors we get).
	// One way of getting that is:
	//	c.Cfg.AdvertiseAddr = "127.0.0.1:0"

	styled := func(s string) string {
		const preamble = `unable to connect or connection lost.

Please check the address and credentials such as certificates \(if attempting to
communicate with a secure cluster\).

`
		return preamble + s
	}

	for _, test := range []struct {
		cmd, expOutPattern string
	}{
		// Error returned from GRPC to internal/client (which has to pass it
		// up the stack as a roachpb.NewError(roachpb.NewSendError(.)).
		// Error returned directly from GRPC.
		{`quit`, styled(
			`Failed to connect to the node: initial connection heartbeat failed: rpc error: ` +
				`code = Unavailable desc = grpc: the connection is unavailable`),
		},
		// Going through the SQL client libraries gives a *net.OpError which
		// we also handle.
		//
		// On *nix, this error is:
		//
		// dial tcp 127.0.0.1:65054: getsockopt: connection refused
		//
		// On Windows, this error is:
		//
		// dial tcp 127.0.0.1:59951: connectex: No connection could be made because the target machine actively refused it.
		//
		// So we look for the common bit.
		{`zone ls`, styled(
			`dial tcp .*: .* refused`),
		},
	} {
		t.Run(test.cmd, func(t *testing.T) {
			out, err := c.RunWithCapture(test.cmd)
			if err != nil {
				t.Fatal(err)
			}
			exp := test.cmd + "\n" + test.expOutPattern
			re := regexp.MustCompile(exp)
			if !re.MatchString(out) {
				t.Errorf("expected '%s' to match pattern:\n%s\ngot:\n%s", test.cmd, exp, out)
			}
		})
	}
}

func Example_ranges() {
	c := newCLITest(cliTestParams{})
	defer c.cleanup()

	c.Run("debug range split ranges3")
	c.Run("debug range ls")

	// Output:
	//debug range split ranges3
	//debug range ls
	///Min-/System/"" [1]
	//	0: node-id=1 store-id=1
	///System/""-/System/NodeLiveness [2]
	//	0: node-id=1 store-id=1
	///System/NodeLiveness-/System/NodeLivenessMax [3]
	//	0: node-id=1 store-id=1
	///System/NodeLivenessMax-/System/tsd [4]
	//	0: node-id=1 store-id=1
	///System/tsd-/System/"tse" [5]
	//	0: node-id=1 store-id=1
	///System/"tse"-"ranges3" [6]
	//	0: node-id=1 store-id=1
	//"ranges3"-/Table/SystemConfigSpan/Start [19]
	//	0: node-id=1 store-id=1
	///Table/SystemConfigSpan/Start-/Table/11 [7]
	//	0: node-id=1 store-id=1
	///Table/11-/Table/12 [8]
	//	0: node-id=1 store-id=1
	///Table/12-/Table/13 [9]
	//	0: node-id=1 store-id=1
	///Table/13-/Table/14 [10]
	//	0: node-id=1 store-id=1
	///Table/14-/Table/15 [11]
	//	0: node-id=1 store-id=1
	///Table/15-/Table/16 [12]
	//	0: node-id=1 store-id=1
	///Table/16-/Table/17 [13]
	//	0: node-id=1 store-id=1
	///Table/17-/Table/18 [14]
	//	0: node-id=1 store-id=1
	///Table/18-/Table/19 [15]
	//	0: node-id=1 store-id=1
	///Table/19-/Table/20 [16]
	//	0: node-id=1 store-id=1
	///Table/20-/Table/21 [17]
	//	0: node-id=1 store-id=1
	///Table/21-/Max [18]
	//	0: node-id=1 store-id=1
	//19 result(s)

}

func Example_logging() {
	c := newCLITest(cliTestParams{})
	defer c.cleanup()

	c.RunWithArgs([]string{"sql", "--logtostderr=false", "-e", "select 1"})
	c.RunWithArgs([]string{"sql", "--log-backtrace-at=foo.go:1", "-e", "select 1"})
	c.RunWithArgs([]string{"sql", "--log-dir=", "-e", "select 1"})
	c.RunWithArgs([]string{"sql", "--logtostderr=true", "-e", "select 1"})
	c.RunWithArgs([]string{"sql", "--verbosity=0", "-e", "select 1"})
	c.RunWithArgs([]string{"sql", "--vmodule=foo=1", "-e", "select 1"})

	// Output:
	// sql --logtostderr=false -e select 1
	// 1
	// 1
	// # 1 row
	// sql --log-backtrace-at=foo.go:1 -e select 1
	// 1
	// 1
	// # 1 row
	// sql --log-dir= -e select 1
	// 1
	// 1
	// # 1 row
	// sql --logtostderr=true -e select 1
	// 1
	// 1
	// # 1 row
	// sql --verbosity=0 -e select 1
	// 1
	// 1
	// # 1 row
	// sql --vmodule=foo=1 -e select 1
	// 1
	// 1
	// # 1 row
}

func Example_max_results() {
	c := newCLITest(cliTestParams{})
	defer c.cleanup()

	c.Run("debug range split max_results3")
	c.Run("debug range split max_results4")
	c.Run("debug range ls --max-results=5")

	// Output:
	//debug range split max_results3
	//debug range split max_results4
	//debug range ls --max-results=5
	///Min-/System/"" [1]
	//	0: node-id=1 store-id=1
	///System/""-/System/NodeLiveness [2]
	//	0: node-id=1 store-id=1
	///System/NodeLiveness-/System/NodeLivenessMax [3]
	//	0: node-id=1 store-id=1
	///System/NodeLivenessMax-/System/tsd [4]
	//	0: node-id=1 store-id=1
	///System/tsd-/System/"tse" [5]
	//	0: node-id=1 store-id=1
	//5 result(s)
}

func Example_zone() {
	c := newCLITest(cliTestParams{})
	defer c.cleanup()

	c.Run("zone ls")
	c.Run("zone set system --file=./testdata/zone_attrs.yaml")
	c.Run("zone ls")
	c.Run("zone get .liveness")
	c.Run("zone get .meta")
	c.Run("zone get system.nonexistent")
	c.Run("zone get system.descriptor")
	c.Run("zone set system.descriptor --file=./testdata/zone_attrs.yaml")
	c.Run("zone set system.namespace --file=./testdata/zone_attrs.yaml")
	c.Run("zone set system.nonexistent --file=./testdata/zone_attrs.yaml")
	c.Run("zone set system --file=./testdata/zone_range_max_bytes.yaml")
	c.Run("zone get system")
	c.Run("zone rm system")
	c.Run("zone ls")
	c.Run("zone rm .default")
	c.Run("zone set .liveness --file=./testdata/zone_range_max_bytes.yaml")
	c.Run("zone set .meta --file=./testdata/zone_range_max_bytes.yaml")
	c.Run("zone set .system --file=./testdata/zone_range_max_bytes.yaml")
	c.Run("zone set .timeseries --file=./testdata/zone_range_max_bytes.yaml")
	c.Run("zone get .system")
	c.Run("zone ls")
	c.Run("zone set .default --file=./testdata/zone_range_max_bytes.yaml")
	c.Run("zone get system")
	c.Run("zone set .default --disable-replication")
	c.Run("zone get system")
	c.Run("zone rm .liveness")
	c.Run("zone rm .meta")
	c.Run("zone rm .system")
	c.Run("zone ls")
	c.Run("zone rm .timeseries")
	c.Run("zone ls")
	c.Run("zone rm .liveness")
	c.Run("zone rm .meta")
	c.Run("zone rm .system")
	c.Run("zone rm .timeseries")
	c.Run("zone set system.jobs@primary --file=./testdata/zone_attrs.yaml")

	// Output:
	// zone ls
	// .default
	// .liveness
	// .meta
	// zone set system --file=./testdata/zone_attrs.yaml
	// range_min_bytes: 1048576
	// range_max_bytes: 67108864
	// gc:
	//   ttlseconds: 90000
	// num_replicas: 1
	// constraints: [us-east-1a, ssd]
	// zone ls
	// .default
	// .liveness
	// .meta
	// system
	// zone get .liveness
	// .liveness
	// range_min_bytes: 1048576
	// range_max_bytes: 67108864
	// gc:
	//   ttlseconds: 600
	// num_replicas: 1
	// constraints: []
	// zone get .meta
	// .meta
	// range_min_bytes: 1048576
	// range_max_bytes: 67108864
	// gc:
	//   ttlseconds: 3600
	// num_replicas: 1
	// constraints: []
	// zone get system.nonexistent
	// pq: relation "system.nonexistent" does not exist
	// zone get system.descriptor
	// system
	// range_min_bytes: 1048576
	// range_max_bytes: 67108864
	// gc:
	//   ttlseconds: 90000
	// num_replicas: 1
	// constraints: [us-east-1a, ssd]
	// zone set system.descriptor --file=./testdata/zone_attrs.yaml
	// pq: cannot set zone configs for system config tables; try setting your config on the entire "system" database instead
	// zone set system.namespace --file=./testdata/zone_attrs.yaml
	// pq: cannot set zone configs for system config tables; try setting your config on the entire "system" database instead
	// zone set system.nonexistent --file=./testdata/zone_attrs.yaml
	// pq: relation "system.nonexistent" does not exist
	// zone set system --file=./testdata/zone_range_max_bytes.yaml
	// range_min_bytes: 1048576
	// range_max_bytes: 134217728
	// gc:
	//   ttlseconds: 90000
	// num_replicas: 3
	// constraints: [us-east-1a, ssd]
	// zone get system
	// system
	// range_min_bytes: 1048576
	// range_max_bytes: 134217728
	// gc:
	//   ttlseconds: 90000
	// num_replicas: 3
	// constraints: [us-east-1a, ssd]
	// zone rm system
	// CONFIGURE ZONE 1
	// zone ls
	// .default
	// .liveness
	// .meta
	// zone rm .default
	// pq: cannot remove default zone
	// zone set .liveness --file=./testdata/zone_range_max_bytes.yaml
	// range_min_bytes: 1048576
	// range_max_bytes: 134217728
	// gc:
	//   ttlseconds: 600
	// num_replicas: 3
	// constraints: []
	// zone set .meta --file=./testdata/zone_range_max_bytes.yaml
	// range_min_bytes: 1048576
	// range_max_bytes: 134217728
	// gc:
	//   ttlseconds: 3600
	// num_replicas: 3
	// constraints: []
	// zone set .system --file=./testdata/zone_range_max_bytes.yaml
	// range_min_bytes: 1048576
	// range_max_bytes: 134217728
	// gc:
	//   ttlseconds: 90000
	// num_replicas: 3
	// constraints: []
	// zone set .timeseries --file=./testdata/zone_range_max_bytes.yaml
	// range_min_bytes: 1048576
	// range_max_bytes: 134217728
	// gc:
	//   ttlseconds: 90000
	// num_replicas: 3
	// constraints: []
	// zone get .system
	// .system
	// range_min_bytes: 1048576
	// range_max_bytes: 134217728
	// gc:
	//   ttlseconds: 90000
	// num_replicas: 3
	// constraints: []
	// zone ls
	// .default
	// .liveness
	// .meta
	// .system
	// .timeseries
	// zone set .default --file=./testdata/zone_range_max_bytes.yaml
	// range_min_bytes: 1048576
	// range_max_bytes: 134217728
	// gc:
	//   ttlseconds: 90000
	// num_replicas: 3
	// constraints: []
	// zone get system
	// .default
	// range_min_bytes: 1048576
	// range_max_bytes: 134217728
	// gc:
	//   ttlseconds: 90000
	// num_replicas: 3
	// constraints: []
	// zone set .default --disable-replication
	// range_min_bytes: 1048576
	// range_max_bytes: 134217728
	// gc:
	//   ttlseconds: 90000
	// num_replicas: 1
	// constraints: []
	// zone get system
	// .default
	// range_min_bytes: 1048576
	// range_max_bytes: 134217728
	// gc:
	//   ttlseconds: 90000
	// num_replicas: 1
	// constraints: []
	// zone rm .liveness
	// CONFIGURE ZONE 1
	// zone rm .meta
	// CONFIGURE ZONE 1
	// zone rm .system
	// CONFIGURE ZONE 1
	// zone ls
	// .default
	// .timeseries
	// zone rm .timeseries
	// CONFIGURE ZONE 1
	// zone ls
	// .default
	// zone rm .liveness
	// CONFIGURE ZONE 0
	// zone rm .meta
	// CONFIGURE ZONE 0
	// zone rm .system
	// CONFIGURE ZONE 0
	// zone rm .timeseries
	// CONFIGURE ZONE 0
	// zone set system.jobs@primary --file=./testdata/zone_attrs.yaml
	// pq: setting zone configs on indexes or partitions requires a CCL binary
}

func Example_sql() {
	c := newCLITest(cliTestParams{})
	defer c.cleanup()

	c.RunWithArgs([]string{"sql", "-e", "show application_name"})
	c.RunWithArgs([]string{"sql", "-e", "create database t; create table t.f (x int, y int); insert into t.f values (42, 69)"})
	c.RunWithArgs([]string{"sql", "-e", "delete from t.f"})
	c.RunWithArgs([]string{"sql", "-e", "select 3", "-e", "select * from t.f"})
	c.RunWithArgs([]string{"sql", "-e", "begin", "-e", "select 3", "-e", "commit"})
	c.RunWithArgs([]string{"sql", "-e", "select * from t.f"})
	c.RunWithArgs([]string{"sql", "--execute=show databases"})
	c.RunWithArgs([]string{"sql", "-e", "select 1; select 2"})
	c.RunWithArgs([]string{"sql", "-e", "select 1; select 2 where false"})
	// CREATE TABLE AS returns a SELECT tag with a row count, check this.
	c.RunWithArgs([]string{"sql", "-e", "create table t.g1 (x int)"})
	c.RunWithArgs([]string{"sql", "-e", "create table t.g2 as select * from generate_series(1,10)"})
	// It must be possible to access pre-defined/virtual tables even if the current database
	// does not exist yet.
	c.RunWithArgs([]string{"sql", "-d", "nonexistent", "-e", "select count(*) from pg_class limit 0"})
	// It must be possible to create the current database after the
	// connection was established.
	c.RunWithArgs([]string{"sql", "-d", "nonexistent", "-e", "create database nonexistent; create table foo(x int); select * from foo"})
	// COPY should return an intelligible error message.
	c.RunWithArgs([]string{"sql", "-e", "copy t.f from stdin"})
	// --echo-sql should print out the SQL statements.
	c.RunWithArgs([]string{"user", "ls", "--echo-sql"})

	// Output:
	// sql -e show application_name
	// application_name
	// cockroach
	// # 1 row
	// sql -e create database t; create table t.f (x int, y int); insert into t.f values (42, 69)
	// INSERT 1
	// sql -e delete from t.f
	// pq: rejected: DELETE without WHERE clause (sql_safe_updates = true)
	// sql -e select 3 -e select * from t.f
	// 3
	// 3
	// # 1 row
	// x	y
	// 42	69
	// # 1 row
	// sql -e begin -e select 3 -e commit
	// BEGIN
	// 3
	// 3
	// # 1 row
	// COMMIT
	// sql -e select * from t.f
	// x	y
	// 42	69
	// # 1 row
	// sql --execute=show databases
	// Database
	// crdb_internal
	// information_schema
	// pg_catalog
	// system
	// t
	// # 5 rows
	// sql -e select 1; select 2
	// 1
	// 1
	// # 1 row
	// 2
	// 2
	// # 1 row
	// sql -e select 1; select 2 where false
	// 1
	// 1
	// # 1 row
	// 2
	// # 0 rows
	// sql -e create table t.g1 (x int)
	// CREATE TABLE
	// sql -e create table t.g2 as select * from generate_series(1,10)
	// SELECT 10
	// sql -d nonexistent -e select count(*) from pg_class limit 0
	// count(*)
	// # 0 rows
	// sql -d nonexistent -e create database nonexistent; create table foo(x int); select * from foo
	// x
	// # 0 rows
	// sql -e copy t.f from stdin
	// woops! COPY has confused this client! Suggestion: use 'psql' for COPY
	// user ls --echo-sql
	// > SHOW USERS
	// username
	// root
	// # 1 row
}

func Example_sql_format() {
	c := newCLITest(cliTestParams{})
	defer c.cleanup()

	c.RunWithArgs([]string{"sql", "-e", "create database t; create table t.times (bare timestamp, withtz timestamptz)"})
	c.RunWithArgs([]string{"sql", "-e", "insert into t.times values ('2016-01-25 10:10:10', '2016-01-25 10:10:10-05:00')"})
	c.RunWithArgs([]string{"sql", "-e", "select * from t.times"})

	// Output:
	// sql -e create database t; create table t.times (bare timestamp, withtz timestamptz)
	// CREATE TABLE
	// sql -e insert into t.times values ('2016-01-25 10:10:10', '2016-01-25 10:10:10-05:00')
	// INSERT 1
	// sql -e select * from t.times
	// bare	withtz
	// 2016-01-25 10:10:10+00:00	2016-01-25 15:10:10+00:00
	// # 1 row
}

func Example_sql_column_labels() {
	c := newCLITest(cliTestParams{})
	defer c.cleanup()

	c.RunWithArgs([]string{"sql", "-e", "create database t; create table t.u (\"\"\"foo\" int, \"\\foo\" int, \"foo\nbar\" int, \"κόσμε\" int, \"a|b\" int, \"܈85\" int)"})
	c.RunWithArgs([]string{"sql", "-e", "insert into t.u values (0, 0, 0, 0, 0, 0)"})
	c.RunWithArgs([]string{"sql", "-e", "show columns from t.u"})
	c.RunWithArgs([]string{"sql", "-e", "select * from t.u"})
	c.RunWithArgs([]string{"sql", "--format=pretty", "-e", "show columns from t.u"})
	c.RunWithArgs([]string{"sql", "--format=pretty", "-e", "select * from t.u"})
	c.RunWithArgs([]string{"sql", "--format=tsv", "-e", "select * from t.u"})
	c.RunWithArgs([]string{"sql", "--format=csv", "-e", "select * from t.u"})
	c.RunWithArgs([]string{"sql", "--format=sql", "-e", "select * from t.u"})
	c.RunWithArgs([]string{"sql", "--format=html", "-e", "select * from t.u"})
	c.RunWithArgs([]string{"sql", "--format=records", "-e", "select * from t.u"})

	// Output:
	// sql -e create database t; create table t.u ("""foo" int, "\foo" int, "foo
	// bar" int, "κόσμε" int, "a|b" int, "܈85" int)
	// CREATE TABLE
	// sql -e insert into t.u values (0, 0, 0, 0, 0, 0)
	// INSERT 1
	// sql -e show columns from t.u
	// Field	Type	Null	Default	Indices
	// """foo"	INT	true	NULL	{}
	// \foo	INT	true	NULL	{}
	// "foo
	// bar"	INT	true	NULL	{}
	// κόσμε	INT	true	NULL	{}
	// a|b	INT	true	NULL	{}
	// ܈85	INT	true	NULL	{}
	// # 6 rows
	// sql -e select * from t.u
	// """foo"	\foo	"""foo\nbar"""	κόσμε	a|b	܈85
	// 0	0	0	0	0	0
	// # 1 row
	// sql --format=pretty -e show columns from t.u
	// +---------+------+------+---------+---------+
	// |  Field  | Type | Null | Default | Indices |
	// +---------+------+------+---------+---------+
	// | "foo    | INT  | true | NULL    | {}      |
	// | \foo    | INT  | true | NULL    | {}      |
	// | foo␤    | INT  | true | NULL    | {}      |
	// | bar     |      |      |         |         |
	// | κόσμε   | INT  | true | NULL    | {}      |
	// | a|b     | INT  | true | NULL    | {}      |
	// | ܈85     | INT  | true | NULL    | {}      |
	// +---------+------+------+---------+---------+
	// (6 rows)
	// sql --format=pretty -e select * from t.u
	// +------+------+------------+-------+-----+-----+
	// | "foo | \foo | "foo\nbar" | κόσμε | a|b | ܈85 |
	// +------+------+------------+-------+-----+-----+
	// |    0 |    0 |          0 |     0 |   0 |   0 |
	// +------+------+------------+-------+-----+-----+
	// (1 row)
	// sql --format=tsv -e select * from t.u
	// """foo"	\foo	"""foo\nbar"""	κόσμε	a|b	܈85
	// 0	0	0	0	0	0
	// # 1 row
	// sql --format=csv -e select * from t.u
	// """foo",\foo,"""foo\nbar""",κόσμε,a|b,܈85
	// 0,0,0,0,0,0
	// # 1 row
	// sql --format=sql -e select * from t.u
	// CREATE TABLE results (
	//   """foo" STRING,
	//   "\foo" STRING,
	//   """foo\nbar""" STRING,
	//   "κόσμε" STRING,
	//   "a|b" STRING,
	//   ܈85 STRING
	// );
	//
	// INSERT INTO results VALUES ('0', '0', '0', '0', '0', '0');
	// sql --format=html -e select * from t.u
	// <table>
	// <thead><tr><th>row</th><th>&#34;foo</th><th>\foo</th><th>&#34;foo\nbar&#34;</th><th>κόσμε</th><th>a|b</th><th>܈85</th></tr></head>
	// <tbody>
	// <tr><td>1</td><td>0</td><td>0</td><td>0</td><td>0</td><td>0</td><td>0</td></tr>
	// </tbody>
	// <tfoot><tr><td colspan=7>1 row</td></tr></tfoot></table>
	// sql --format=records -e select * from t.u
	// -[ RECORD 1 ]
	// "foo       | 0
	// \foo       | 0
	// "foo\nbar" | 0
	// κόσμε      | 0
	// a|b        | 0
	// ܈85        | 0
}

func Example_sql_empty_table() {
	c := newCLITest(cliTestParams{})
	defer c.cleanup()

	c.RunWithArgs([]string{"sql", "-e", "create database t;" +
		"create table t.norows(x int);" +
		"create table t.nocolsnorows();" +
		"create table t.nocols(); insert into t.nocols(rowid) values (1),(2),(3);"})
	for _, table := range []string{"norows", "nocols", "nocolsnorows"} {
		for _, format := range []string{"pretty", "tsv", "csv", "sql", "html", "raw", "records"} {
			c.RunWithArgs([]string{"sql", "--format=" + format, "-e", "select * from t." + table})
		}
	}

	// Output:
	// sql -e create database t;create table t.norows(x int);create table t.nocolsnorows();create table t.nocols(); insert into t.nocols(rowid) values (1),(2),(3);
	// INSERT 3
	// sql --format=pretty -e select * from t.norows
	// +---+
	// | x |
	// +---+
	// +---+
	// (0 rows)
	// sql --format=tsv -e select * from t.norows
	// x
	// # 0 rows
	// sql --format=csv -e select * from t.norows
	// x
	// # 0 rows
	// sql --format=sql -e select * from t.norows
	// CREATE TABLE results (
	//   x STRING
	// );
	//
	// sql --format=html -e select * from t.norows
	// <table>
	// <thead><tr><th>row</th><th>x</th></tr></head>
	// </tbody>
	// <tfoot><tr><td colspan=2>0 rows</td></tr></tfoot></table>
	// sql --format=raw -e select * from t.norows
	// # 1 column
	// # 0 rows
	// sql --format=records -e select * from t.norows
	// sql --format=pretty -e select * from t.nocols
	// --
	// (3 rows)
	// sql --format=tsv -e select * from t.nocols
	// # no columns
	// # empty
	// # empty
	// # empty
	// # 3 rows
	// sql --format=csv -e select * from t.nocols
	// # no columns
	// # empty
	// # empty
	// # empty
	// # 3 rows
	// sql --format=sql -e select * from t.nocols
	// CREATE TABLE results (
	// );
	//
	// INSERT INTO results(rowid) VALUES (DEFAULT);
	// INSERT INTO results(rowid) VALUES (DEFAULT);
	// INSERT INTO results(rowid) VALUES (DEFAULT);
	// sql --format=html -e select * from t.nocols
	// <table>
	// <thead><tr><th>row</th></tr></head>
	// <tbody>
	// <tr><td>1</td></tr>
	// <tr><td>2</td></tr>
	// <tr><td>3</td></tr>
	// </tbody>
	// <tfoot><tr><td colspan=1>3 rows</td></tr></tfoot></table>
	// sql --format=raw -e select * from t.nocols
	// # 0 columns
	// # row 1
	// # row 2
	// # row 3
	// # 3 rows
	// sql --format=records -e select * from t.nocols
	// (3 rows)
	// sql --format=pretty -e select * from t.nocolsnorows
	// --
	// (0 rows)
	// sql --format=tsv -e select * from t.nocolsnorows
	// # no columns
	// # 0 rows
	// sql --format=csv -e select * from t.nocolsnorows
	// # no columns
	// # 0 rows
	// sql --format=sql -e select * from t.nocolsnorows
	// CREATE TABLE results (
	// );
	//
	// sql --format=html -e select * from t.nocolsnorows
	// <table>
	// <thead><tr><th>row</th></tr></head>
	// </tbody>
	// <tfoot><tr><td colspan=1>0 rows</td></tr></tfoot></table>
	// sql --format=raw -e select * from t.nocolsnorows
	// # 0 columns
	// # 0 rows
	// sql --format=records -e select * from t.nocolsnorows
	// (0 rows)
}

func Example_sql_table() {
	c := newCLITest(cliTestParams{})
	defer c.cleanup()

	c.RunWithArgs([]string{"sql", "-e", "create database t; create table t.t (s string, d string);"})
	c.RunWithArgs([]string{"sql", "-e", "insert into t.t values (e'foo', 'printable ASCII')"})
	c.RunWithArgs([]string{"sql", "-e", "insert into t.t values (e'\"foo', 'printable ASCII with quotes')"})
	c.RunWithArgs([]string{"sql", "-e", "insert into t.t values (e'\\\\foo', 'printable ASCII with backslash')"})
	c.RunWithArgs([]string{"sql", "-e", "insert into t.t values (e'foo\\x0abar', 'non-printable ASCII')"})
	c.RunWithArgs([]string{"sql", "-e", "insert into t.t values ('κόσμε', 'printable UTF8')"})
	c.RunWithArgs([]string{"sql", "-e", "insert into t.t values (e'\\xc3\\xb1', 'printable UTF8 using escapes')"})
	c.RunWithArgs([]string{"sql", "-e", "insert into t.t values (e'\\x01', 'non-printable UTF8 string')"})
	c.RunWithArgs([]string{"sql", "-e", "insert into t.t values (e'\\xdc\\x88\\x38\\x35', 'UTF8 string with RTL char')"})
	c.RunWithArgs([]string{"sql", "-e", "insert into t.t values (e'\\xc3\\x28', 'non-UTF8 string')"})
	c.RunWithArgs([]string{"sql", "-e", "insert into t.t values (e'a\\tb\\tc\\n12\\t123123213\\t12313', 'tabs')"})
	c.RunWithArgs([]string{"sql", "-e", "select * from t.t"})
	c.RunWithArgs([]string{"sql", "--format=pretty", "-e", "select * from t.t"})
	c.RunWithArgs([]string{"sql", "--format=tsv", "-e", "select * from t.t"})
	c.RunWithArgs([]string{"sql", "--format=csv", "-e", "select * from t.t"})
	c.RunWithArgs([]string{"sql", "--format=sql", "-e", "select * from t.t"})
	c.RunWithArgs([]string{"sql", "--format=html", "-e", "select * from t.t"})
	c.RunWithArgs([]string{"sql", "--format=raw", "-e", "select * from t.t"})
	c.RunWithArgs([]string{"sql", "--format=records", "-e", "select * from t.t"})
	c.RunWithArgs([]string{"sql", "--format=pretty", "-e", "select '  hai' as x"})
	c.RunWithArgs([]string{"sql", "--format=pretty", "-e", "explain(indent) select s from t.t union all select s from t.t"})

	// Output:
	// sql -e create database t; create table t.t (s string, d string);
	// CREATE TABLE
	// sql -e insert into t.t values (e'foo', 'printable ASCII')
	// INSERT 1
	// sql -e insert into t.t values (e'"foo', 'printable ASCII with quotes')
	// INSERT 1
	// sql -e insert into t.t values (e'\\foo', 'printable ASCII with backslash')
	// INSERT 1
	// sql -e insert into t.t values (e'foo\x0abar', 'non-printable ASCII')
	// INSERT 1
	// sql -e insert into t.t values ('κόσμε', 'printable UTF8')
	// INSERT 1
	// sql -e insert into t.t values (e'\xc3\xb1', 'printable UTF8 using escapes')
	// INSERT 1
	// sql -e insert into t.t values (e'\x01', 'non-printable UTF8 string')
	// INSERT 1
	// sql -e insert into t.t values (e'\xdc\x88\x38\x35', 'UTF8 string with RTL char')
	// INSERT 1
	// sql -e insert into t.t values (e'\xc3\x28', 'non-UTF8 string')
	// pq: invalid UTF-8 byte sequence
	// DETAIL: source SQL:
	// insert into t.t values (e'\xc3\x28', 'non-UTF8 string')
	//                         ^
	// HINT: try \h VALUES
	// sql -e insert into t.t values (e'a\tb\tc\n12\t123123213\t12313', 'tabs')
	// INSERT 1
	// sql -e select * from t.t
	// s	d
	// foo	printable ASCII
	// """foo"	printable ASCII with quotes
	// \foo	printable ASCII with backslash
	// "foo
	// bar"	non-printable ASCII
	// κόσμε	printable UTF8
	// ñ	printable UTF8 using escapes
	// """\x01"""	non-printable UTF8 string
	// ܈85	UTF8 string with RTL char
	// "a	b	c
	// 12	123123213	12313"	tabs
	// # 9 rows
	// sql --format=pretty -e select * from t.t
	// +--------------------------------+--------------------------------+
	// |               s                |               d                |
	// +--------------------------------+--------------------------------+
	// | foo                            | printable ASCII                |
	// | "foo                           | printable ASCII with quotes    |
	// | \foo                           | printable ASCII with backslash |
	// | foo␤                           | non-printable ASCII            |
	// | bar                            |                                |
	// | κόσμε                          | printable UTF8                 |
	// | ñ                              | printable UTF8 using escapes   |
	// | "\x01"                         | non-printable UTF8 string      |
	// | ܈85                            | UTF8 string with RTL char      |
	// | a   b         c␤               | tabs                           |
	// | 12  123123213 12313            |                                |
	// +--------------------------------+--------------------------------+
	// (9 rows)
	// sql --format=tsv -e select * from t.t
	// s	d
	// foo	printable ASCII
	// """foo"	printable ASCII with quotes
	// \foo	printable ASCII with backslash
	// "foo
	// bar"	non-printable ASCII
	// κόσμε	printable UTF8
	// ñ	printable UTF8 using escapes
	// """\x01"""	non-printable UTF8 string
	// ܈85	UTF8 string with RTL char
	// "a	b	c
	// 12	123123213	12313"	tabs
	// # 9 rows
	// sql --format=csv -e select * from t.t
	// s,d
	// foo,printable ASCII
	// """foo",printable ASCII with quotes
	// \foo,printable ASCII with backslash
	// "foo
	// bar",non-printable ASCII
	// κόσμε,printable UTF8
	// ñ,printable UTF8 using escapes
	// """\x01""",non-printable UTF8 string
	// ܈85,UTF8 string with RTL char
	// "a	b	c
	// 12	123123213	12313",tabs
	// # 9 rows
	// sql --format=sql -e select * from t.t
	// CREATE TABLE results (
	//   s STRING,
	//   d STRING
	// );
	//
	// INSERT INTO results VALUES ('foo', 'printable ASCII');
	// INSERT INTO results VALUES ('"foo', 'printable ASCII with quotes');
	// INSERT INTO results VALUES (e'\\foo', 'printable ASCII with backslash');
	// INSERT INTO results VALUES (e'foo\nbar', 'non-printable ASCII');
	// INSERT INTO results VALUES (e'\u03BA\U00001F79\u03C3\u03BC\u03B5', 'printable UTF8');
	// INSERT INTO results VALUES (e'\u00F1', 'printable UTF8 using escapes');
	// INSERT INTO results VALUES (e'"\\x01"', 'non-printable UTF8 string');
	// INSERT INTO results VALUES (e'\u070885', 'UTF8 string with RTL char');
	// INSERT INTO results VALUES (e'a\tb\tc\n12\t123123213\t12313', 'tabs');
	// sql --format=html -e select * from t.t
	// <table>
	// <thead><tr><th>row</th><th>s</th><th>d</th></tr></head>
	// <tbody>
	// <tr><td>1</td><td>foo</td><td>printable ASCII</td></tr>
	// <tr><td>2</td><td>&#34;foo</td><td>printable ASCII with quotes</td></tr>
	// <tr><td>3</td><td>\foo</td><td>printable ASCII with backslash</td></tr>
	// <tr><td>4</td><td>foo<br/>bar</td><td>non-printable ASCII</td></tr>
	// <tr><td>5</td><td>κόσμε</td><td>printable UTF8</td></tr>
	// <tr><td>6</td><td>ñ</td><td>printable UTF8 using escapes</td></tr>
	// <tr><td>7</td><td>&#34;\x01&#34;</td><td>non-printable UTF8 string</td></tr>
	// <tr><td>8</td><td>܈85</td><td>UTF8 string with RTL char</td></tr>
	// <tr><td>9</td><td>a	b	c<br/>12	123123213	12313</td><td>tabs</td></tr>
	// </tbody>
	// <tfoot><tr><td colspan=3>9 rows</td></tr></tfoot></table>
	// sql --format=raw -e select * from t.t
	// # 2 columns
	// # row 1
	// ## 3
	// foo
	// ## 15
	// printable ASCII
	// # row 2
	// ## 4
	// "foo
	// ## 27
	// printable ASCII with quotes
	// # row 3
	// ## 4
	// \foo
	// ## 30
	// printable ASCII with backslash
	// # row 4
	// ## 7
	// foo
	// bar
	// ## 19
	// non-printable ASCII
	// # row 5
	// ## 11
	// κόσμε
	// ## 14
	// printable UTF8
	// # row 6
	// ## 2
	// ñ
	// ## 28
	// printable UTF8 using escapes
	// # row 7
	// ## 6
	// "\x01"
	// ## 25
	// non-printable UTF8 string
	// # row 8
	// ## 4
	// ܈85
	// ## 25
	// UTF8 string with RTL char
	// # row 9
	// ## 24
	// a	b	c
	// 12	123123213	12313
	// ## 4
	// tabs
	// # 9 rows
	// sql --format=records -e select * from t.t
	// -[ RECORD 1 ]
	// s | foo
	// d | printable ASCII
	// -[ RECORD 2 ]
	// s | "foo
	// d | printable ASCII with quotes
	// -[ RECORD 3 ]
	// s | \foo
	// d | printable ASCII with backslash
	// -[ RECORD 4 ]
	// s | foo
	//   | bar
	// d | non-printable ASCII
	// -[ RECORD 5 ]
	// s | κόσμε
	// d | printable UTF8
	// -[ RECORD 6 ]
	// s | ñ
	// d | printable UTF8 using escapes
	// -[ RECORD 7 ]
	// s | "\x01"
	// d | non-printable UTF8 string
	// -[ RECORD 8 ]
	// s | ܈85
	// d | UTF8 string with RTL char
	// -[ RECORD 9 ]
	// s | a	b	c
	//   | 12	123123213	12313
	// d | tabs
	// sql --format=pretty -e select '  hai' as x
	// +-------+
	// |   x   |
	// +-------+
	// |   hai |
	// +-------+
	// (1 row)
	// sql --format=pretty -e explain(indent) select s from t.t union all select s from t.t
	// +-------+--------+-------+--------------------+
	// | Level |  Type  | Field |    Description     |
	// +-------+--------+-------+--------------------+
	// |     0 | append |       |  -> append         |
	// |     1 | render |       |    -> render       |
	// |     2 | scan   |       |       -> scan      |
	// |     2 |        | table |          t@primary |
	// |     2 |        | spans |          ALL       |
	// |     1 | render |       |    -> render       |
	// |     2 | scan   |       |       -> scan      |
	// |     2 |        | table |          t@primary |
	// |     2 |        | spans |          ALL       |
	// +-------+--------+-------+--------------------+
	// (9 rows)
}

func Example_user() {
	c := newCLITest(cliTestParams{})
	defer c.cleanup()

	c.Run("user ls")
	c.Run("user ls --format=pretty")
	c.Run("user ls --format=tsv")
	c.Run("user set FOO")
	c.RunWithArgs([]string{"sql", "-e", "create user if not exists 'FOO'"})
	c.Run("user set Foo")
	c.Run("user set fOo")
	c.Run("user set foO")
	c.Run("user set foo")
	c.Run("user set _foo")
	c.Run("user set f_oo")
	c.Run("user set foo_")
	c.Run("user set ,foo")
	c.Run("user set f,oo")
	c.Run("user set foo,")
	c.Run("user set 0foo")
	c.Run("user set foo0")
	c.Run("user set f0oo")
	c.Run("user set foofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoof")
	c.Run("user set foofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoo")
	c.Run("user set Ομηρος")
	// Try some reserved keywords.
	c.Run("user set and")
	c.Run("user set table")
	// Don't use get, since the output of hashedPassword is random.
	// c.Run("user get foo")
	c.Run("user ls --format=pretty")
	c.Run("user rm foo")
	c.Run("user ls --format=pretty")

	// Output:
	// user ls
	// username
	// root
	// # 1 row
	// user ls --format=pretty
	// +----------+
	// | username |
	// +----------+
	// | root     |
	// +----------+
	// (1 row)
	// user ls --format=tsv
	// username
	// root
	// # 1 row
	// user set FOO
	// CREATE USER 1
	// sql -e create user if not exists 'FOO'
	// CREATE USER 0
	// user set Foo
	// CREATE USER 0
	// user set fOo
	// CREATE USER 0
	// user set foO
	// CREATE USER 0
	// user set foo
	// CREATE USER 0
	// user set _foo
	// CREATE USER 1
	// user set f_oo
	// CREATE USER 1
	// user set foo_
	// CREATE USER 1
	// user set ,foo
	// pq: username ",foo" invalid; usernames are case insensitive, must start with a letter or underscore, may contain letters, digits or underscores, and must not exceed 63 characters
	// user set f,oo
	// pq: username "f,oo" invalid; usernames are case insensitive, must start with a letter or underscore, may contain letters, digits or underscores, and must not exceed 63 characters
	// user set foo,
	// pq: username "foo," invalid; usernames are case insensitive, must start with a letter or underscore, may contain letters, digits or underscores, and must not exceed 63 characters
	// user set 0foo
	// pq: username "0foo" invalid; usernames are case insensitive, must start with a letter or underscore, may contain letters, digits or underscores, and must not exceed 63 characters
	// user set foo0
	// CREATE USER 1
	// user set f0oo
	// CREATE USER 1
	// user set foofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoof
	// pq: username "foofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoof" invalid; usernames are case insensitive, must start with a letter or underscore, may contain letters, digits or underscores, and must not exceed 63 characters
	// user set foofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoo
	// CREATE USER 1
	// user set Ομηρος
	// CREATE USER 1
	// user set and
	// CREATE USER 1
	// user set table
	// CREATE USER 1
	// user ls --format=pretty
	// +-----------------------------------------------------------------+
	// |                            username                             |
	// +-----------------------------------------------------------------+
	// | _foo                                                            |
	// | and                                                             |
	// | f0oo                                                            |
	// | f_oo                                                            |
	// | foo                                                             |
	// | foo0                                                            |
	// | foo_                                                            |
	// | foofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoo |
	// | root                                                            |
	// | table                                                           |
	// | ομηρος                                                          |
	// +-----------------------------------------------------------------+
	// (11 rows)
	// user rm foo
	// DROP USER 1
	// user ls --format=pretty
	// +-----------------------------------------------------------------+
	// |                            username                             |
	// +-----------------------------------------------------------------+
	// | _foo                                                            |
	// | and                                                             |
	// | f0oo                                                            |
	// | f_oo                                                            |
	// | foo0                                                            |
	// | foo_                                                            |
	// | foofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoo |
	// | root                                                            |
	// | table                                                           |
	// | ομηρος                                                          |
	// +-----------------------------------------------------------------+
	// (10 rows)
}

func Example_cert() {
	c := newCLITest(cliTestParams{})
	defer c.cleanup()

	c.RunWithCAArgs([]string{"cert", "create-client", "foo"})
	c.RunWithCAArgs([]string{"cert", "create-client", "Ομηρος"})
	c.RunWithCAArgs([]string{"cert", "create-client", "0foo"})

	// Output:
	// cert create-client foo
	// cert create-client Ομηρος
	// cert create-client 0foo
	// failed to generate client certificate and key: username "0foo" invalid; usernames are case insensitive, must start with a letter or underscore, may contain letters, digits or underscores, and must not exceed 63 characters
}

// TestFlagUsage is a basic test to make sure the fragile
// help template does not break.
func TestFlagUsage(t *testing.T) {
	defer leaktest.AfterTest(t)()

	expUsage := `Usage:
  cockroach [command]

Available Commands:
  start       start a node
  init        initialize a cluster
  cert        create ca, node, and client certs
  quit        drain and shutdown node

  sql         open a sql shell
  user        get, set, list and remove users
  zone        get, set, list and remove zones
  node        list, inspect or remove nodes
  dump        dump sql tables

  gen         generate auxiliary files
  version     output version information
  debug       debugging commands
  help        Help about any command

Flags:
  -h, --help                             help for cockroach
      --log-backtrace-at traceLocation   when logging hits line file:N, emit a stack trace (default :0)
      --log-dir string                   if non-empty, write log files in this directory
      --log-dir-max-size bytes           maximum combined size of all log files (default 100 MiB)
      --log-file-max-size bytes          maximum size of each log file (default 10 MiB)
      --log-file-verbosity Severity      minimum verbosity of messages written to the log file (default INFO)
      --logtostderr Severity[=DEFAULT]   logs at or above this threshold go to stderr (default NONE)
      --no-color                         disable standard error log colorization

Use "cockroach [command] --help" for more information about a command.
`
	helpExpected := fmt.Sprintf("CockroachDB command-line interface and server.\n\n%s", expUsage)
	badFlagExpected := fmt.Sprintf("%s\nError: unknown flag: --foo\n", expUsage)

	testCases := []struct {
		flags    []string
		expErr   bool
		expected string
	}{
		{[]string{"help"}, false, helpExpected},    // request help specifically
		{[]string{"--foo"}, true, badFlagExpected}, // unknown flag
	}
	for _, test := range testCases {
		t.Run(strings.Join(test.flags, ","), func(t *testing.T) {
			// Override os.Stdout or os.Stderr with our own.
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			cockroachCmd.SetOutput(w)
			defer cockroachCmd.SetOutput(nil)

			done := make(chan error)
			var buf bytes.Buffer
			// copy the output in a separate goroutine so printing can't block indefinitely.
			go func() {
				// Copy reads 'r' until EOF is reached.
				_, err := io.Copy(&buf, r)
				done <- err
			}()

			if err := Run(test.flags); err != nil && !test.expErr {
				t.Error(err)
			}

			// back to normal state
			w.Close()
			if err := <-done; err != nil {
				t.Fatal(err)
			}

			// Filter out all test flags.
			testFlagRE := regexp.MustCompile(`--(test\.|verbosity|vmodule)`)
			lines := strings.Split(buf.String(), "\n")
			final := []string{}
			for _, l := range lines {
				if testFlagRE.MatchString(l) {
					continue
				}
				final = append(final, l)
			}
			got := strings.Join(final, "\n")

			if got != test.expected {
				t.Errorf("got:\n%s\n----\nexpected:\n%s", got, test.expected)
			}
		})
	}
}

func Example_node() {
	c := newCLITest(cliTestParams{})
	defer c.cleanup()

	// Refresh time series data, which is required to retrieve stats.
	if err := c.WriteSummaries(); err != nil {
		log.Fatalf(context.Background(), "Couldn't write stats summaries: %s", err)
	}

	c.Run("node ls")
	c.Run("node ls --format=pretty")
	c.Run("node status 10000")

	// Output:
	// node ls
	// id
	// 1
	// # 1 row
	// node ls --format=pretty
	// +----+
	// | id |
	// +----+
	// |  1 |
	// +----+
	// (1 row)
	// node status 10000
	// Error: node 10000 doesn't exist
}

func TestCLITimeout(t *testing.T) {
	defer leaktest.AfterTest(t)()

	c := newCLITest(cliTestParams{})
	defer c.cleanup()

	// Wrap the meat of the test in a retry loop. Setting a timeout like this is
	// racy as the operation may have succeeded by the time the scheduler gives
	// the timeout a chance to have an effect.
	testutils.SucceedsSoon(t, func() error {
		out, err := c.RunWithCapture("node status 1 --timeout 1ns")
		if err != nil {
			t.Fatal(err)
		}

		const exp = `node status 1 --timeout 1ns
operation timed out.

context deadline exceeded
`
		if out != exp {
			err := errors.Errorf("unexpected output:\n%q\nwanted:\n%q", out, exp)
			t.Log(err)
			return err
		}
		return nil
	})
}

func TestNodeStatus(t *testing.T) {
	defer leaktest.AfterTest(t)()

	start := timeutil.Now()
	c := newCLITest(cliTestParams{})
	defer c.cleanup()

	// Refresh time series data, which is required to retrieve stats.
	if err := c.WriteSummaries(); err != nil {
		t.Fatalf("couldn't write stats summaries: %s", err)
	}

	out, err := c.RunWithCapture("node status 1 --format=pretty")
	if err != nil {
		t.Fatal(err)
	}
	checkNodeStatus(t, c, out, start)

	out, err = c.RunWithCapture("node status --ranges --format=pretty")
	if err != nil {
		t.Fatal(err)
	}
	checkNodeStatus(t, c, out, start)

	out, err = c.RunWithCapture("node status --stats --format=pretty")
	if err != nil {
		t.Fatal(err)
	}
	checkNodeStatus(t, c, out, start)

	out, err = c.RunWithCapture("node status --ranges --stats --format=pretty")
	if err != nil {
		t.Fatal(err)
	}
	checkNodeStatus(t, c, out, start)

	out, err = c.RunWithCapture("node status --decommission --format=pretty")
	if err != nil {
		t.Fatal(err)
	}
	checkNodeStatus(t, c, out, start)

	out, err = c.RunWithCapture("node status --ranges --stats --decommission --format=pretty")
	if err != nil {
		t.Fatal(err)
	}
	checkNodeStatus(t, c, out, start)

	out, err = c.RunWithCapture("node status --all --format=pretty")
	if err != nil {
		t.Fatal(err)
	}
	checkNodeStatus(t, c, out, start)

	out, err = c.RunWithCapture("node status --format=pretty")
	if err != nil {
		t.Fatal(err)
	}
	checkNodeStatus(t, c, out, start)
}

func checkNodeStatus(t *testing.T, c cliTest, output string, start time.Time) {
	buf := bytes.NewBufferString(output)
	s := bufio.NewScanner(buf)

	type testCase struct {
		name   string
		idx    int
		maxval int64
	}

	// Skip command line.
	if !s.Scan() {
		t.Fatalf("Couldn't skip command line: %s", s.Err())
	}

	checkSeparatorLine(t, s)

	// check column names.
	if !s.Scan() {
		t.Fatalf("Error reading column names: %s", s.Err())
	}
	cols, err := extractFields(s.Text())
	if err != nil {
		t.Fatalf("%s", err)
	}
	if !reflect.DeepEqual(cols, getStatusNodeHeaders()) {
		t.Fatalf("columns (%s) don't match expected (%s)", cols, getStatusNodeHeaders())
	}

	checkSeparatorLine(t, s)

	// Check node status.
	if !s.Scan() {
		t.Fatalf("error reading node status: %s", s.Err())
	}
	fields, err := extractFields(s.Text())
	if err != nil {
		t.Fatalf("%s", err)
	}

	nodeID := c.Gossip().NodeID.Get()
	nodeIDStr := strconv.FormatInt(int64(nodeID), 10)
	if a, e := fields[0], nodeIDStr; a != e {
		t.Errorf("node id (%s) != expected (%s)", a, e)
	}

	nodeAddr, err := c.Gossip().GetNodeIDAddress(nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if a, e := fields[1], nodeAddr.String(); a != e {
		t.Errorf("node address (%s) != expected (%s)", a, e)
	}

	// Verify Build Tag.
	if a, e := fields[2], build.GetInfo().Tag; a != e {
		t.Errorf("build tag (%s) != expected (%s)", a, e)
	}

	// Verify that updated_at and started_at are reasonably recent.
	// CircleCI can be very slow. This was flaky at 5s.
	checkTimeElapsed(t, fields[3], 15*time.Second, start)
	checkTimeElapsed(t, fields[4], 15*time.Second, start)

	testcases := []testCase{}

	// We're skipping over the first 5 default fields such as node id and
	// address. They don't need closer checks.
	baseIdx := len(baseNodeColumnHeaders)

	// Adding fields that need verification for --range flag.
	if nodeCtx.statusShowRanges || nodeCtx.statusShowAll {
		testcases = append(testcases,
			testCase{"leader_ranges", baseIdx, 3},
			testCase{"repl_ranges", baseIdx + 1, 3},
			testCase{"avail_ranges", baseIdx + 2, 3},
		)

		// Ranges actually adds 5 fields, but we only need to check
		// the 3 above.
		baseIdx += len(statusNodesColumnHeadersForRanges)
	}

	// Adding fields that need verification for --stats flag.
	if nodeCtx.statusShowStats || nodeCtx.statusShowAll {
		testcases = append(testcases,
			testCase{"live_bytes", baseIdx, 100000},
			testCase{"key_bytes", baseIdx + 1, 30000},
			testCase{"value_bytes", baseIdx + 2, 100000},
			testCase{"intent_bytes", baseIdx + 3, 30000},
			testCase{"system_bytes", baseIdx + 4, 30000},
		)
		baseIdx += len(statusNodesColumnHeadersForStats)
	}

	for _, tc := range testcases {
		val, err := strconv.ParseInt(fields[tc.idx], 10, 64)
		if err != nil {
			t.Errorf("couldn't parse %s '%s': %v", tc.name, fields[tc.idx], err)
			continue
		}
		if val < 0 {
			t.Errorf("value for %s (%d) cannot be less than 0", tc.name, val)
			continue
		}
		if val > tc.maxval {
			t.Errorf("value for %s (%d) greater than max (%d)", tc.name, val, tc.maxval)
		}
	}

	checkSeparatorLine(t, s)
}

var separatorLineExp = regexp.MustCompile(`[\+-]+$`)

func checkSeparatorLine(t *testing.T, s *bufio.Scanner) {
	if !s.Scan() {
		t.Fatalf("error reading separator line: %s", s.Err())
	}
	if !separatorLineExp.MatchString(s.Text()) {
		t.Fatalf("separator line not found: %s", s.Text())
	}
}

// checkRecentTime produces a test error if the time is not within the specified number
// of seconds of the given start time.
func checkTimeElapsed(t *testing.T, timeStr string, elapsed time.Duration, start time.Time) {
	// Truncate start time, because the CLI currently outputs times with a second-level
	// granularity.
	start = start.Truncate(time.Second)
	tm, err := time.ParseInLocation(localTimeFormat, timeStr, start.Location())
	if err != nil {
		t.Errorf("couldn't parse time '%s': %s", timeStr, err)
		return
	}
	end := start.Add(elapsed)
	if tm.Before(start) || tm.After(end) {
		t.Errorf("time (%s) not within range [%s,%s]", tm, start, end)
	}
}

// extractFields extracts the fields from a pretty-printed row of SQL output,
// discarding excess whitespace and column separators.
func extractFields(line string) ([]string, error) {
	fields := strings.Split(line, "|")
	// fields has two extra entries, one for the empty token to the left of the first
	// |, and another empty one to the right of the final |. So, we need to take those
	// out.
	if a, e := len(fields), len(getStatusNodeHeaders())+2; a != e {
		return nil, errors.Errorf("can't extract fields: # of fields (%d) != expected (%d)", a, e)
	}
	fields = fields[1 : len(fields)-1]
	var r []string
	for _, f := range fields {
		r = append(r, strings.TrimSpace(f))
	}
	return r, nil
}

func TestGenMan(t *testing.T) {
	defer leaktest.AfterTest(t)()

	// Generate man pages in a temp directory.
	manpath, err := ioutil.TempDir("", "TestGenMan")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.RemoveAll(manpath); err != nil {
			t.Errorf("couldn't remove temporary directory %s: %s", manpath, err)
		}
	}()
	if err := Run([]string{"gen", "man", "--path=" + manpath}); err != nil {
		t.Fatal(err)
	}

	// Ensure we have a sane number of man pages.
	count := 0
	err = filepath.Walk(manpath, func(path string, info os.FileInfo, err error) error {
		if strings.HasSuffix(path, ".1") && !info.IsDir() {
			count++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if min := 20; count < min {
		t.Errorf("number of man pages (%d) < minimum (%d)", count, min)
	}
}

func TestGenAutocomplete(t *testing.T) {
	defer leaktest.AfterTest(t)()

	// Get a unique path to which we can write our autocomplete files.
	acdir, err := ioutil.TempDir("", "TestGenAutoComplete")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.RemoveAll(acdir); err != nil {
			t.Errorf("couldn't remove temporary directory %s: %s", acdir, err)
		}
	}()

	const minsize = 25000
	acpath := filepath.Join(acdir, "cockroach.bash")

	if err := Run([]string{"gen", "autocomplete", "--out=" + acpath}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(acpath)
	if err != nil {
		t.Fatal(err)
	}
	if size := info.Size(); size < minsize {
		t.Fatalf("autocomplete file size (%d) < minimum (%d)", size, minsize)
	}
}

func TestJunkPositionalArguments(t *testing.T) {
	defer leaktest.AfterTest(t)()

	c := newCLITest(cliTestParams{t: t, noServer: true})
	defer c.cleanup()

	for i, test := range []string{
		"start junk",
		"sql junk",
		"gen man junk",
		"gen autocomplete junk",
		"gen example-data file junk",
	} {
		out, err := c.RunWithCapture(test)
		if err != nil {
			t.Fatal(errors.Wrap(err, strconv.Itoa(i)))
		}
		exp := test + "\ninvalid arguments\n"
		if exp != out {
			t.Errorf("expected:\n%s\ngot:\n%s", exp, out)
		}
	}
}

func TestZip(t *testing.T) {
	defer leaktest.AfterTest(t)()

	c := newCLITest(cliTestParams{})
	defer c.cleanup()

	out, err := c.RunWithCapture("debug zip " + os.DevNull)
	if err != nil {
		t.Fatal(err)
	}

	const expected = `debug zip ` + os.DevNull + `
writing ` + os.DevNull + `
  debug/events
  debug/liveness
  debug/settings
  debug/nodes/1/status
  debug/nodes/1/gossip
  debug/nodes/1/stacks
  debug/nodes/1/ranges/1
  debug/nodes/1/ranges/2
  debug/nodes/1/ranges/3
  debug/nodes/1/ranges/4
  debug/nodes/1/ranges/5
  debug/nodes/1/ranges/6
  debug/nodes/1/ranges/7
  debug/nodes/1/ranges/8
  debug/nodes/1/ranges/9
  debug/nodes/1/ranges/10
  debug/nodes/1/ranges/11
  debug/nodes/1/ranges/12
  debug/nodes/1/ranges/13
  debug/nodes/1/ranges/14
  debug/nodes/1/ranges/15
  debug/nodes/1/ranges/16
  debug/nodes/1/ranges/17
  debug/nodes/1/ranges/18
  debug/schema/system@details
  debug/schema/system/descriptor
  debug/schema/system/eventlog
  debug/schema/system/jobs
  debug/schema/system/lease
  debug/schema/system/locations
  debug/schema/system/namespace
  debug/schema/system/rangelog
  debug/schema/system/settings
  debug/schema/system/table_statistics
  debug/schema/system/ui
  debug/schema/system/users
  debug/schema/system/web_sessions
  debug/schema/system/zones
`

	if out != expected {
		t.Errorf("expected:\n%s\ngot:\n%s", expected, out)
	}
}

func Example_in_memory() {
	spec, err := base.NewStoreSpec("type=mem,size=1GiB")
	if err != nil {
		panic(err)
	}
	c := newCLITest(cliTestParams{
		storeSpecs: []base.StoreSpec{spec},
	})
	defer c.cleanup()

	// Test some sql to ensure that the in memory store is working.
	c.RunWithArgs([]string{"sql", "-e", "create database t; create table t.f (x int, y int); insert into t.f values (42, 69)"})
	c.RunWithArgs([]string{"node", "ls"})

	// Output:
	// sql -e create database t; create table t.f (x int, y int); insert into t.f values (42, 69)
	// INSERT 1
	// node ls
	// id
	// 1
	// # 1 row
}
