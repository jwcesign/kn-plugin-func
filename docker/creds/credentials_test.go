//go:build !integration
// +build !integration

package creds_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	fn "knative.dev/kn-plugin-func"
	"knative.dev/kn-plugin-func/docker"
	"knative.dev/kn-plugin-func/docker/creds"
	. "knative.dev/kn-plugin-func/testing"

	"github.com/docker/docker-credential-helpers/credentials"
)

func Test_registryEquals(t *testing.T) {
	tests := []struct {
		name string
		urlA string
		urlB string
		want bool
	}{
		{"no port matching host", "quay.io", "quay.io", true},
		{"non-matching host added sub-domain", "sub.quay.io", "quay.io", false},
		{"non-matching host different sub-domain", "sub.quay.io", "sub3.quay.io", false},
		{"localhost", "localhost", "localhost", true},
		{"localhost with standard ports", "localhost:80", "localhost:443", false},
		{"localhost with matching port", "https://localhost:1234", "http://localhost:1234", true},
		{"localhost with match by default port 80", "http://localhost", "localhost:80", true},
		{"localhost with match by default port 443", "https://localhost", "localhost:443", true},
		{"localhost with mismatch by non-default port 5000", "https://localhost", "localhost:5000", false},
		{"localhost with match by empty ports", "https://localhost", "http://localhost", true},
		{"docker.io matching host https", "https://docker.io", "docker.io", true},
		{"docker.io matching host http", "http://docker.io", "docker.io", true},
		{"docker.io with path", "docker.io/v1/", "docker.io", true},
		{"docker.io with protocol and path", "https://docker.io/v1/", "docker.io", true},
		{"docker.io with subdomain index.", "https://index.docker.io/v1/", "docker.io", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := creds.RegistryEquals(tt.urlA, tt.urlB); got != tt.want {
				t.Errorf("to2ndLevelDomain() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCheckAuth(t *testing.T) {

	const (
		uname        = "testuser"
		pwd          = "testpwd"
		incorrectPwd = "badpwd"
	)

	localhost, localhostTLS, stopServer := startServer(t, uname, pwd)
	t.Cleanup(stopServer)

	_, portTLS, err := net.SplitHostPort(localhostTLS)
	if err != nil {
		t.Fatal(err)
	}

	nonLocalhostTLS := "test.io:" + portTLS

	type args struct {
		ctx      context.Context
		username string
		password string
		registry string
	}
	tests := []struct {
		name    string
		args    args
		wantErr bool
	}{
		{
			name: "correct credentials localhost no-TLS",
			args: args{
				ctx:      context.Background(),
				username: uname,
				password: pwd,
				registry: localhost,
			},
			wantErr: false,
		},
		{
			name: "correct credentials localhost",
			args: args{
				ctx:      context.Background(),
				username: uname,
				password: pwd,
				registry: localhostTLS,
			},
			wantErr: false,
		},

		{
			name: "correct credentials non-localhost",
			args: args{
				ctx:      context.Background(),
				username: uname,
				password: pwd,
				registry: nonLocalhostTLS,
			},
			wantErr: false,
		},
		{
			name: "incorrect credentials localhost no-TLS",
			args: args{
				ctx:      context.Background(),
				username: uname,
				password: incorrectPwd,
				registry: localhost,
			},
			wantErr: true,
		},
		{
			name: "incorrect credentials localhost",
			args: args{
				ctx:      context.Background(),
				username: uname,
				password: incorrectPwd,
				registry: localhostTLS,
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := docker.Credentials{
				Username: tt.args.username,
				Password: tt.args.password,
			}
			if err := creds.CheckAuth(tt.args.ctx, tt.args.registry, c, http.DefaultTransport); (err != nil) != tt.wantErr {
				t.Errorf("CheckAuth() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestCheckAuthEmptyCreds(t *testing.T) {

	localhost, _, stopServer := startServer(t, "", "")
	t.Cleanup(stopServer)
	err := creds.CheckAuth(context.Background(), localhost, docker.Credentials{}, http.DefaultTransport)
	if err != nil {
		t.Error(err)
	}
}

func startServer(t *testing.T, uname, pwd string) (addr, addrTLS string, stopServer func()) {
	// TODO: this should be refactored to use OS-chosen ports so as not to
	// fail when a user is running a Function on the default port.)
	listener, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	addr = listener.Addr().String()

	listenerTLS, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	addrTLS = listenerTLS.Addr().String()

	handler := http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
		if uname == "" || pwd == "" {
			resp.WriteHeader(http.StatusOK)
			return
		}
		// TODO add also test for token based auth
		resp.Header().Add("WWW-Authenticate", "basic")
		if u, p, ok := req.BasicAuth(); ok {
			if u == uname && p == pwd {
				resp.WriteHeader(http.StatusOK)
				return
			}
		}
		resp.WriteHeader(http.StatusUnauthorized)
	})

	var randReader io.Reader = rand.Reader

	caPublicKey, caPrivateKey, err := ed25519.GenerateKey(randReader)
	if err != nil {
		t.Fatal(err)
	}

	ca := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: "localhost",
		},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		DNSNames:              []string{"localhost", "test.io"},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		IsCA:                  true,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		ExtraExtensions:       []pkix.Extension{},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}

	caBytes, err := x509.CreateCertificate(randReader, ca, ca, caPublicKey, caPrivateKey)
	if err != nil {
		t.Fatal(err)
	}

	ca, err = x509.ParseCertificate(caBytes)
	if err != nil {
		t.Fatal(err)
	}

	cert := tls.Certificate{
		Certificate: [][]byte{caBytes},
		PrivateKey:  caPrivateKey,
		Leaf:        ca,
	}

	server := http.Server{
		Handler: handler,
		TLSConfig: &tls.Config{
			ServerName:   "localhost",
			Certificates: []tls.Certificate{cert},
		},
	}

	go func() {
		err := server.ServeTLS(listenerTLS, "", "")
		if err != nil && !strings.Contains(err.Error(), "Server closed") {
			panic(err)
		}
	}()

	go func() {
		err := server.Serve(listener)
		if err != nil && !strings.Contains(err.Error(), "Server closed") {
			panic(err)
		}
	}()

	// make the testing CA trusted by default HTTP transport/client
	oldDefaultTransport := http.DefaultTransport
	newDefaultTransport := http.DefaultTransport.(*http.Transport).Clone()
	http.DefaultTransport = newDefaultTransport
	caPool := x509.NewCertPool()
	caPool.AddCert(ca)
	newDefaultTransport.TLSClientConfig.RootCAs = caPool
	dc := newDefaultTransport.DialContext
	newDefaultTransport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		h, p, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		if h == "test.io" {
			h = "localhost"
		}
		addr = net.JoinHostPort(h, p)
		return dc(ctx, network, addr)
	}

	return addr, addrTLS, func() {
		err := server.Shutdown(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		http.DefaultTransport = oldDefaultTransport
	}
}

const (
	dockerIoUser    = "testUser1"
	dockerIoUserPwd = "goodPwd1"
	quayIoUser      = "testUser2"
	quayIoUserPwd   = "goodPwd2"
)

type Credentials = docker.Credentials

func TestNewCredentialsProvider(t *testing.T) {
	defer withCleanHome(t)()

	helperWithQuayIO := newInMemoryHelper()

	err := helperWithQuayIO.Add(&credentials.Credentials{
		ServerURL: "quay.io",
		Username:  quayIoUser,
		Secret:    quayIoUserPwd,
	})
	if err != nil {
		t.Fatal(err)
	}

	type args struct {
		promptUser        creds.CredentialsCallback
		verifyCredentials creds.VerifyCredentialsCallback
		additionalLoaders []creds.CredentialsCallback
		registry          string
		setUpEnv          setUpEnv
	}
	tests := []struct {
		name string
		args args
		want Credentials
	}{
		{
			name: "test user callback correct password on first try",
			args: args{
				promptUser:        correctPwdCallback,
				verifyCredentials: correctVerifyCbk,
				registry:          "docker.io",
			},
			want: Credentials{Username: dockerIoUser, Password: dockerIoUserPwd},
		},
		{
			name: "test user callback correct password on second try",
			args: args{
				promptUser:        pwdCbkFirstWrongThenCorrect(t),
				verifyCredentials: correctVerifyCbk,
				registry:          "docker.io",
			},
			want: Credentials{Username: dockerIoUser, Password: dockerIoUserPwd},
		},
		{
			name: "get quay-io credentials with func config populated",
			args: args{
				promptUser:        pwdCbkThatShallNotBeCalled(t),
				verifyCredentials: correctVerifyCbk,
				registry:          "quay.io",
				setUpEnv:          withPopulatedFuncAuthConfig,
			},
			want: Credentials{Username: quayIoUser, Password: quayIoUserPwd},
		},
		{
			name: "get docker-io credentials with func config populated",
			args: args{
				promptUser:        pwdCbkThatShallNotBeCalled(t),
				verifyCredentials: correctVerifyCbk,
				registry:          "docker.io",
				setUpEnv:          withPopulatedFuncAuthConfig,
			},
			want: Credentials{Username: dockerIoUser, Password: dockerIoUserPwd},
		},
		{
			name: "get quay-io credentials with docker config populated",
			args: args{
				promptUser:        pwdCbkThatShallNotBeCalled(t),
				verifyCredentials: correctVerifyCbk,
				registry:          "quay.io",
				setUpEnv: all(
					withPopulatedDockerAuthConfig,
					setUpMockHelper("docker-credential-mock", helperWithQuayIO)),
			},
			want: Credentials{Username: quayIoUser, Password: quayIoUserPwd},
		},
		{
			name: "get docker-io credentials with docker config populated",
			args: args{
				promptUser:        pwdCbkThatShallNotBeCalled(t),
				verifyCredentials: correctVerifyCbk,
				registry:          "docker.io",
				setUpEnv:          withPopulatedDockerAuthConfig,
			},
			want: Credentials{Username: dockerIoUser, Password: dockerIoUserPwd},
		},
		{
			name: "get docker-io credentials from custom loader",
			args: args{
				promptUser:        pwdCbkThatShallNotBeCalled(t),
				verifyCredentials: correctVerifyCbk,
				registry:          "docker.io",
				additionalLoaders: []creds.CredentialsCallback{correctPwdCallback},
			},
			want: Credentials{Username: dockerIoUser, Password: dockerIoUserPwd},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer cleanUpConfigs(t)

			if tt.args.setUpEnv != nil {
				defer tt.args.setUpEnv(t)()
			}

			credentialsProvider := creds.NewCredentialsProvider(
				creds.WithPromptForCredentials(tt.args.promptUser),
				creds.WithVerifyCredentials(tt.args.verifyCredentials),
				creds.WithAdditionalCredentialLoaders(tt.args.additionalLoaders...))
			got, err := credentialsProvider(context.Background(), tt.args.registry)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if got != tt.want {
				t.Errorf("credentialsProvider() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNewCredentialsProviderEmptyCreds(t *testing.T) {
	credentialsProvider := creds.NewCredentialsProvider(creds.WithVerifyCredentials(func(ctx context.Context, registry string, credentials docker.Credentials) error {
		if registry == "localhost:5000" && credentials == (docker.Credentials{}) {
			return nil
		}
		t.Fatal("unreachable")
		return nil
	}))
	c, err := credentialsProvider(context.Background(), "localhost:5000")
	if err != nil {
		t.Error(err)
	}
	if c != (docker.Credentials{}) {
		t.Error("unexpected credentials")
	}
}

func TestCredentialsProviderSavingFromUserInput(t *testing.T) {
	defer withCleanHome(t)()

	helper := newInMemoryHelper()
	defer setUpMockHelper("docker-credential-mock", helper)(t)()

	var pwdCbkInvocations int
	pwdCbk := func(r string) (Credentials, error) {
		pwdCbkInvocations++
		return correctPwdCallback(r)
	}

	chooseNoStore := func(available []string) (string, error) {
		if len(available) < 1 {
			t.Errorf("this should have been invoked with non empty list")
		}
		return "", nil
	}
	chooseMockStore := func(available []string) (string, error) {
		if len(available) < 1 {
			t.Errorf("this should have been invoked with non empty list")
		}
		return "docker-credential-mock", nil
	}
	shallNotBeInvoked := func(available []string) (string, error) {
		t.Fatal("this choose helper callback shall not be invoked")
		return "", errors.New("this callback shall not be invoked")
	}

	credentialsProvider := creds.NewCredentialsProvider(
		creds.WithPromptForCredentials(pwdCbk),
		creds.WithVerifyCredentials(correctVerifyCbk),
		creds.WithPromptForCredentialStore(chooseNoStore))
	_, err := credentialsProvider(context.Background(), "docker.io")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
		return
	}

	// now credentials should not be saved because no helper was provided
	l, err := helper.List()
	if err != nil {
		t.Fatal(err)
	}
	credsInStore := len(l)
	if credsInStore != 0 {
		t.Errorf("expected to have zero credentials in store, but has: %d", credsInStore)
	}
	credentialsProvider = creds.NewCredentialsProvider(
		creds.WithPromptForCredentials(pwdCbk),
		creds.WithVerifyCredentials(correctVerifyCbk),
		creds.WithPromptForCredentialStore(chooseMockStore))
	_, err = credentialsProvider(context.Background(), "docker.io")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
		return
	}
	if pwdCbkInvocations != 2 {
		t.Errorf("the pwd callback should have been invoked exactly twice but was invoked %d time", pwdCbkInvocations)
	}

	// now credentials should be saved in the mock secure store
	l, err = helper.List()
	if err != nil {
		t.Fatal(err)
	}
	credsInStore = len(l)
	if len(l) != 1 {
		t.Errorf("expected to have exactly one credentials in store, but has: %d", credsInStore)
	}
	credentialsProvider = creds.NewCredentialsProvider(
		creds.WithPromptForCredentials(pwdCbkThatShallNotBeCalled(t)),
		creds.WithVerifyCredentials(correctVerifyCbk),
		creds.WithPromptForCredentialStore(shallNotBeInvoked))
	_, err = credentialsProvider(context.Background(), "docker.io")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
		return
	}
}

func cleanUpConfigs(t *testing.T) {
	home, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}

	os.RemoveAll(fn.ConfigPath())

	os.RemoveAll(filepath.Join(home, ".docker"))
}

type setUpEnv = func(t *testing.T) func()

func withPopulatedDockerAuthConfig(t *testing.T) func() {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	dockerConfigDir := filepath.Join(home, ".docker")
	dockerConfigPath := filepath.Join(dockerConfigDir, "config.json")
	err = os.MkdirAll(filepath.Dir(dockerConfigPath), 0700)
	if err != nil {
		t.Fatal(err)
	}

	configJSON := `{
	"auths": {
		"docker.io": { "auth": "%s" },
		"quay.io": {}
	},
	"credsStore": "mock"
}`
	configJSON = fmt.Sprintf(configJSON, base64.StdEncoding.EncodeToString([]byte(dockerIoUser+":"+dockerIoUserPwd)))

	err = ioutil.WriteFile(dockerConfigPath, []byte(configJSON), 0600)
	if err != nil {
		t.Fatal(err)
	}

	return func() {

		os.RemoveAll(dockerConfigDir)
	}
}

func withPopulatedFuncAuthConfig(t *testing.T) func() {
	t.Helper()

	var err error

	authConfig := filepath.Join(fn.ConfigPath(), "auth.json")
	err = os.MkdirAll(filepath.Dir(authConfig), 0700)
	if err != nil {
		t.Fatal(err)
	}

	authJSON := `{
	"auths": {
		"docker.io": { "auth": "%s" },
		"quay.io":   { "auth": "%s" }
	}
}`
	authJSON = fmt.Sprintf(authJSON,
		base64.StdEncoding.EncodeToString([]byte(dockerIoUser+":"+dockerIoUserPwd)),
		base64.StdEncoding.EncodeToString([]byte(quayIoUser+":"+quayIoUserPwd)))

	err = ioutil.WriteFile(authConfig, []byte(authJSON), 0600)
	if err != nil {
		t.Fatal(err)
	}
	return func() {
		os.RemoveAll(fn.ConfigPath())
	}
}

func pwdCbkThatShallNotBeCalled(t *testing.T) creds.CredentialsCallback {
	t.Helper()
	return func(registry string) (Credentials, error) {
		return Credentials{}, errors.New("this pwd cbk code shall not be called")
	}
}

func pwdCbkFirstWrongThenCorrect(t *testing.T) func(registry string) (Credentials, error) {
	t.Helper()
	var firstInvocation bool
	return func(registry string) (Credentials, error) {
		if registry != "docker.io" && registry != "quay.io" {
			return Credentials{}, fmt.Errorf("unexpected registry: %s", registry)
		}
		if firstInvocation {
			firstInvocation = false
			return Credentials{Username: dockerIoUser, Password: "badPwd"}, nil
		}
		return correctPwdCallback(registry)
	}
}

func correctPwdCallback(registry string) (Credentials, error) {
	if registry == "docker.io" {
		return Credentials{Username: dockerIoUser, Password: dockerIoUserPwd}, nil
	}
	if registry == "quay.io" {
		return Credentials{Username: quayIoUser, Password: quayIoUserPwd}, nil
	}
	return Credentials{}, errors.New("this cbk don't know the pwd")
}

func correctVerifyCbk(ctx context.Context, registry string, credentials Credentials) error {
	username, password := credentials.Username, credentials.Password
	if username == dockerIoUser && password == dockerIoUserPwd && registry == "docker.io" {
		return nil
	}
	if username == quayIoUser && password == quayIoUserPwd && registry == "quay.io" {
		return nil
	}
	return creds.ErrUnauthorized
}

func withCleanHome(t *testing.T) func() {
	t.Helper()
	homeName := "HOME"
	if runtime.GOOS == "windows" {
		homeName = "USERPROFILE"
	}
	tmpHome := t.TempDir()
	oldHome, hadHome := os.LookupEnv(homeName)
	os.Setenv(homeName, tmpHome)

	oldXDGConfigHome, hadXDGConfigHome := os.LookupEnv("XDG_CONFIG_HOME")

	if runtime.GOOS == "linux" {
		os.Setenv("XDG_CONFIG_HOME", filepath.Join(tmpHome, ".config"))
	}

	return func() {
		if hadHome {
			os.Setenv(homeName, oldHome)
		} else {
			os.Unsetenv(homeName)
		}
		if hadXDGConfigHome {
			os.Setenv("XDG_CONFIG_HOME", oldXDGConfigHome)
		} else {
			os.Unsetenv("XDG_CONFIG_HOME")
		}
	}
}

func handlerForCredHelper(t *testing.T, credHelper credentials.Helper) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		defer request.Body.Close()

		var err error
		var outBody interface{}

		uri := strings.Trim(request.RequestURI, "/")

		var serverURL string
		if uri == "get" || uri == "erase" {
			data, err := ioutil.ReadAll(request.Body)
			if err != nil {
				writer.WriteHeader(http.StatusInternalServerError)
				return
			}
			serverURL = string(data)
			serverURL = strings.Trim(serverURL, "\n\r\t ")
		}

		switch uri {
		case "list":
			var list map[string]string
			list, err = credHelper.List()
			if err == nil {
				outBody = &list
			}
		case "store":
			creds := credentials.Credentials{}
			dec := json.NewDecoder(request.Body)
			err = dec.Decode(&creds)
			if err != nil {
				break
			}
			err = credHelper.Add(&creds)
		case "get":
			var user, secret string
			user, secret, err = credHelper.Get(serverURL)
			if err == nil {
				outBody = &credentials.Credentials{ServerURL: serverURL, Username: user, Secret: secret}
			}
		case "erase":
			err = credHelper.Delete(serverURL)
		default:
			writer.WriteHeader(http.StatusNotFound)
			return
		}

		if err != nil {
			if credentials.IsErrCredentialsNotFound(err) {
				writer.WriteHeader(http.StatusNotFound)
			} else {
				writer.WriteHeader(http.StatusInternalServerError)
				writer.Header().Add("Content-Type", "text/plain")
				fmt.Fprintf(writer, "error: %+v\n", err)
			}
			return
		}

		if outBody != nil {
			var data []byte
			data, err = json.Marshal(outBody)
			if err != nil {
				writer.WriteHeader(http.StatusInternalServerError)
				return
			}
			writer.Header().Add("Content-Type", "application/json")
			_, err = writer.Write(data)
			if err != nil {
				t.Fatal(err)
			}
		}
	})

}

// Go source code of mock docker-credential-helper implementation.
// Its storage is backed by inMemoryHelper instantiated in test and exposed via HTTP.
const helperGoSrc = `package main

import (
	"errors"
	"io"
	"log"
	"net/http"
	"os"
)

func main() {
	var (
		baseURL = os.Getenv("HELPER_BASE_URL")
		resp *http.Response
		err error
	)
	cmd := os.Args[1]
	switch cmd {
	case "list":
		resp, err = http.Get(baseURL + "/" + cmd)
		if err != nil {
			log.Fatal(err)
		}
		io.Copy(os.Stdout, resp.Body)
	case "get", "erase":
		resp, err = http.Post(baseURL+ "/" + cmd, "text/plain", os.Stdin)
		if err != nil {
			log.Fatal(err)
		}
		io.Copy(os.Stdout, resp.Body)
	case "store":
		resp, err = http.Post(baseURL+ "/" + cmd, "application/json", os.Stdin)
		if err != nil {
			log.Fatal(err)
		}
	default:
		log.Fatal(errors.New("unknown cmd: " + cmd))
	}
	if resp.StatusCode != http.StatusOK {
		log.Fatal(errors.New(resp.Status))
	}
	return
}
`

// Creates executable with name determined by the helperName parameter and puts it on $PATH.
//
// The executable behaves like docker credential helper (https://github.com/docker/docker-credential-helpers).
//
// The content of the store presented by the executable is backed by the helper parameter.
func setUpMockHelper(helperName string, helper credentials.Helper) func(t *testing.T) func() {
	var cleanUps []func()
	return func(t *testing.T) func() {

		cleanUps = append(cleanUps, WithExecutable(t, helperName, helperGoSrc))

		listener, err := net.Listen("tcp", "localhost:0")
		if err != nil {
			t.Fatal(err)
		}

		cleanUps = append(cleanUps, func() {
			_ = listener.Close()
		})

		baseURL := fmt.Sprintf("http://%s", listener.Addr().String())
		cleanUps = append(cleanUps, WithEnvVar(t, "HELPER_BASE_URL", baseURL))

		server := http.Server{Handler: handlerForCredHelper(t, helper)}
		servErrChan := make(chan error)
		go func() {
			servErrChan <- server.Serve(listener)
		}()

		cleanUps = append(cleanUps, func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
			defer cancel()
			_ = server.Shutdown(ctx)
			e := <-servErrChan
			if !errors.Is(e, http.ErrServerClosed) {
				t.Fatal(e)
			}
		})

		return func() {
			for i := len(cleanUps) - 1; i <= 0; i-- {
				cleanUps[i]()
			}
		}
	}
}

// combines multiple setUp routines into one setUp routine
func all(fns ...setUpEnv) setUpEnv {
	return func(t *testing.T) func() {
		t.Helper()
		var cleanUps []func()
		for _, fn := range fns {
			cleanUps = append(cleanUps, fn(t))
		}

		return func() {
			for i := len(cleanUps) - 1; i >= 0; i-- {
				cleanUps[i]()
			}
		}
	}
}

func newInMemoryHelper() *inMemoryHelper {
	return &inMemoryHelper{lock: &sync.Mutex{}, credentials: make(map[string]credentials.Credentials)}
}

type inMemoryHelper struct {
	credentials map[string]credentials.Credentials
	lock        sync.Locker
}

func (i *inMemoryHelper) Add(credentials *credentials.Credentials) error {
	i.lock.Lock()
	defer i.lock.Unlock()

	i.credentials[credentials.ServerURL] = *credentials

	return nil
}

func (i *inMemoryHelper) Get(serverURL string) (string, string, error) {
	i.lock.Lock()
	defer i.lock.Unlock()

	if result, ok := i.credentials[serverURL]; ok {
		return result.Username, result.Secret, nil
	}

	return "", "", credentials.NewErrCredentialsNotFound()
}

func (i *inMemoryHelper) List() (map[string]string, error) {
	i.lock.Lock()
	defer i.lock.Unlock()

	result := make(map[string]string, len(i.credentials))

	for k, v := range i.credentials {
		result[k] = v.Username
	}

	return result, nil
}

func (i *inMemoryHelper) Delete(serverURL string) error {
	i.lock.Lock()
	defer i.lock.Unlock()

	if _, ok := i.credentials[serverURL]; ok {
		delete(i.credentials, serverURL)
		return nil
	}

	return credentials.NewErrCredentialsNotFound()
}
