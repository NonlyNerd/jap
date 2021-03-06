package main

// BUG(ssw): JAP does not support TLS. To access the service with TLS (which you
//           really should be doing), use a reverse proxy such as Nginx.

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/rpc"
	"net/rpc/jsonrpc"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/jitsi/jap"
	"golang.org/x/net/context"
	"golang.org/x/net/netutil"
	"golang.org/x/net/trace"
	"golang.org/x/text/language"
)

var (
	addr, pubDir, tmplDir, keyPath     string
	googleClientSecret, googleClientID string
	originURL                          string
	maxConns, rpcRetries               int
	rpcAddr, rpcMethod, rpcCodec       string

	tmpl *template.Template
)

func init() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "%s\n\nUsage of %s:\n", help, os.Args[0])
		flag.PrintDefaults()
	}

	flag.StringVar(&addr, "http", ":http-alt", "The address to listen on.")
	flag.StringVar(&pubDir, "public", "public/", "A directory containing static files to serve.")
	flag.StringVar(&tmplDir, "templates", "templates/", "A directory containing templates to render.")
	flag.StringVar(&keyPath, "key", os.Getenv("JAP_PRIVATE_KEY_PATH"), "An RSA private key in PEM format to use for signing tokens. Defaults to $JAP_PRIVATE_KEY_PATH.")
	flag.StringVar(&originURL, "origin", "", "A domain that the /login endpoint will send a postMessage too (eg. https://meet.jit.si).")
	flag.StringVar(&rpcAddr, "rpcaddr", "", "An address that can be used to make RPC calls to verify permissions for a user.")
	flag.StringVar(&rpcMethod, "rpc", "Permissions.Check", "The RPC call to make to rcpaddr. This should be a function that takes a string (the token) and replies with a boolean. It should be compatible with Go's net/rpc package.")
	flag.StringVar(&rpcCodec, "rpccodec", "gob", `The type of RPC call to make (either "gob" for Go gobs or "json" for JSON-RPC).`)
	flag.IntVar(&rpcRetries, "rpcretries", 3, "The number of times to retry making RPC calls.")
	flag.IntVar(&maxConns, "maxconns", 0, "The maximum number of connections to service at once or 0 for unlimited.")
	flag.Parse()

	googleClientID = os.Getenv("GOOGLE_CLIENT_ID")
	googleClientSecret = os.Getenv("GOOGLE_CLIENT_SECRET")

	loadTemplates()

	sigs := make(chan os.Signal)
	signal.Notify(sigs, syscall.SIGHUP)

	// Handle signals
	go func() {
		var s os.Signal
		for {
			s = <-sigs
			switch s {
			case syscall.SIGHUP:
				log.Println("Received SIGHUP: reloading templates…")
				loadTemplates()
			}
		}
	}()
}

// Load all templates found in the tmplDir directory; if any of them contain
// errors, panic.
func loadTemplates() {
	files, err := filepath.Glob(filepath.Join(tmplDir, "*.tmpl"))
	switch {
	case err != nil:
		log.Fatal(err)
	case len(files) < 1:
		log.Fatalf("No templates found in %s", tmplDir)
	}
	tmpl = template.Must(template.New("jap").ParseFiles(files...))
}

func loadRSAKeyFromPEM(pembytes []byte) (*rsa.PrivateKey, error) {
	if len(pembytes) == 0 {
		return nil, errors.New("No pem data found")
	}
	var blk *pem.Block
decodeloop:
	for {
		blk, pembytes = pem.Decode(pembytes)
		switch {
		case blk == nil:
			break decodeloop
		case blk.Type == "RSA PRIVATE KEY":
			return x509.ParsePKCS1PrivateKey(blk.Bytes)
		}
	}
	return nil, fmt.Errorf("No RSA private key found in pem file %s", keyPath)
}

// TODO: Add incremental backoff and dialer retries.
func dialRPC() (rpcClient *rpc.Client, err error) {
	if rpcAddr != "" && rpcMethod != "" {
		log.Printf("Dialing RPC server at %s…", rpcAddr)
		switch rpcCodec {
		case "json":
			return jsonrpc.Dial("tcp", rpcAddr)
		case "gob":
			return rpc.DialHTTP("tcp", rpcAddr)
		default:
			return nil, fmt.Errorf("No such RPC codec %s", rpcCodec)
		}
	}
	return nil, nil
}

func main() {
	if keyPath == "" && os.Getenv("JAP_PRIVATE_KEY") == "" {
		log.Fatalf("No private key specified. Try: %s -help", os.Args[0])
	}

	var pembytes []byte
	var err error
	if keyenv := os.Getenv("JAP_PRIVATE_KEY"); keyenv != "" {
		pembytes = []byte(keyenv)
	} else {
		pembytes, err = ioutil.ReadFile(keyPath)
	}
	if err != nil {
		log.Fatal(err)
	}
	key, err := loadRSAKeyFromPEM(pembytes)
	if err != nil {
		log.Fatal(err)
	}

	var rpcClient *rpc.Client
	if rpcClient, err = dialRPC(); err != nil {
		log.Println("Failed to dial RPC server:", err)
	}

	log.Printf("Starting server on %s…\n", addr)

	var permCheck jap.PermissionChecker
	if rpcAddr != "" && rpcMethod != "" {
		permCheck = func(tok string) (b bool, err error) {
			for i := 0; i < rpcRetries; i++ {
				err = rpcClient.Call(rpcMethod, tok, &b)
				log.Println("CHECKED:", b, err)
				switch err {
				case nil:
					return b, err
				case rpc.ErrShutdown, io.ErrUnexpectedEOF:
					rpcClient, _ = dialRPC()
					continue
				}
			}
			return false, err
		}
	}
	http.HandleFunc("/googlelogin", jap.GoogleLogin(
		jap.NewCIDContext(context.Background(), googleClientID), key, permCheck))
	http.HandleFunc("/login", loginHandler(context.Background()))
	if pubDir != "" {
		http.Handle("/", http.StripPrefix("/", http.FileServer(http.Dir(pubDir))))
	}

	l, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}
	if maxConns > 0 {
		l = netutil.LimitListener(l, maxConns)
	}
	log.Fatal(http.Serve(l, nil))
}

func loginHandler(ctx context.Context) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		tr := trace.New("jap.login", r.URL.Path)
		defer tr.Finish()

		tr.LazyPrintf("Executing login.tmpl…")
		err := tmpl.ExecuteTemplate(w, "login.tmpl", Login{
			Lang:           language.English,
			GoogleClientID: googleClientID,
			TargetOrigin:   originURL,
		})
		if err != nil {
			tr.LazyPrintf("Error exeuting login.tmpl:", err.Error())
			tr.SetError()
			return
		}
		tr.LazyPrintf("Done executing login.tmpl…")
	}
}

// Login represents all the information we need to show the login window.
type Login struct {
	Lang           language.Tag
	GoogleClientID string
	TargetOrigin   string
}
