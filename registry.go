package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/bmizerany/mc"
	"github.com/elazarl/goproxy"
	"github.com/jackc/pgx"
)

var (
	cn    *mc.Conn
	pool  *pgx.ConnPool
	proxy *goproxy.ProxyHttpServer
)

func urlHasPrefix(prefix string) goproxy.ReqConditionFunc {
	return func(req *http.Request, ctx *goproxy.ProxyCtx) bool {
		isGET := req.Method == http.MethodGet
		hasPrefix := strings.HasPrefix(req.URL.Path, prefix)
		isSearch := strings.HasPrefix(req.URL.Path, "/packages/search/")
		return isGET && hasPrefix && !isSearch
	}
}

func pathIs(path string) goproxy.ReqConditionFunc {
	return func(req *http.Request, ctx *goproxy.ProxyCtx) bool {
		return req.Method == http.MethodGet && req.URL.Path == path
	}
}

func getEnv(key, def string) string {
	k := os.Getenv(key)
	if k == "" {
		return def
	}
	return k
}

func main() {
	memcachedURL := getEnv("MEMCACHEDCLOUD_SERVERS", "localhost:11211")
	var err error
	cn, err = mc.Dial("tcp", memcachedURL)
	if err != nil {
		log.Fatalf("Memcached connection error: %s", err)
	}

	memcachedUsername := os.Getenv("MEMCACHEDCLOUD_USERNAME")
	memcachedPassword := os.Getenv("MEMCACHEDCLOUD_PASSWORD")
	if memcachedUsername != "" && memcachedPassword != "" {
		if err := cn.Auth(memcachedUsername, memcachedPassword); err != nil {
			log.Fatalf("Memcached auth error: %s", err)
		}
	}

	pgxcfg, err := pgx.ParseURI(os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatalf("Parse URI error: %s", err)
	}
	pool, err = pgx.NewConnPool(pgx.ConnPoolConfig{
		ConnConfig:     pgxcfg,
		MaxConnections: 20,
		AfterConnect: func(conn *pgx.Conn) error {
			_, err := conn.Prepare("getPackage", `SELECT name, url FROM packages WHERE name = $1`)
			return err
		},
	})
	if err != nil {
		log.Fatalf("Connection error: %s", err)
	}
	defer pool.Close()

	binary, err := exec.LookPath("node")
	if err != nil {
		log.Fatalf("Could not lookup node path: %s", err)
	}

	cmd := exec.Command(binary, "--expose_gc", "index.js")
	env := os.Environ()
	env = append(env, "PORT=3001")
	cmd.Env = env
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		log.Fatalf("Could not start node: %s", err)
	}
	go func() {
		if err := cmd.Wait(); err != nil {
			log.Fatalf("Node process failed: %s", err)
		}
	}()

	proxy = goproxy.NewProxyHttpServer()
	proxy.Verbose = false
	proxy.NonproxyHandler = http.HandlerFunc(nonProxy)

	proxy.OnRequest().DoFunc(
		func(r *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
			if r.Method == "GET" && r.Host != "registry.bower.io" && r.Host != "components.bower.io" {
				if strings.HasPrefix(r.URL.Path, "/packages/search/") {

					response := goproxy.NewResponse(r, "application/json", http.StatusOK, `[{"name":"deprecated","url":"This bower version is deprecated. Please update it: npm update -g bower"}]`)
					return r, response
				}
				time.Sleep(10 * time.Second)
				response := goproxy.NewResponse(r, "application/json", http.StatusPermanentRedirect, "")
				target := "https://registry.bower.io" + r.URL.Path
				if len(r.URL.RawQuery) > 0 {
					target += "?" + r.URL.RawQuery
				}
				response.Header.Set("Location", target)
				return r, response
			}

			return r, nil
		})

	proxy.OnRequest(pathIs("/packages")).DoFunc(listPackages)
	proxy.OnRequest(urlHasPrefix("/packages/")).DoFunc(getPackage)

	port := getEnv("PORT", "3000")
	log.Println("Starting web server at port", port)
	log.Fatal(http.ListenAndServe(":"+port, proxy))
}

func nonProxy(w http.ResponseWriter, req *http.Request) {
	req.URL.Scheme = "http"
	req.URL.Host = "localhost:3001"
	proxy.ServeHTTP(w, req)
}

type Package struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

func getPackage(r *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
	elements := strings.Split(r.URL.Path, "/")
	packageName := elements[len(elements)-1]

	var name, url string
	if err := pool.QueryRow("getPackage", packageName).Scan(&name, &url); err != nil {
		if err == pgx.ErrNoRows {
			return r, goproxy.NewResponse(r, "text/html", http.StatusNotFound, "Package not found")
		}
		return r, goproxy.NewResponse(r, "text/html", http.StatusInternalServerError, "Internal server error")
	}

	data, err := json.Marshal(Package{Name: name, URL: url})
	if err != nil {
		return r, goproxy.NewResponse(r, "text/html", http.StatusInternalServerError, "Internal server error")
	}
	response := goproxy.NewResponse(r, "application/json", http.StatusOK, string(data))
	response.Header.Add("Cache-Control", "public, max-age=604800")
	return r, response
}

func listPackages(r *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
	val, _, _, err := cn.Get("packages")
	if err != nil {
		return r, nil
	}
	response := goproxy.NewResponse(r, "application/json", http.StatusOK, val)
	response.Header.Add("Cache-Control", "public, max-age=604800")
	return r, response
}
