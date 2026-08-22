package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

func runRuntime(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: agentos runtime <activate|cordon|drain> -pool ID -version N")
	}
	status := map[string]string{"activate": "ACTIVE", "cordon": "CORDONED", "drain": "DRAINING"}[args[0]]
	if status == "" {
		return fmt.Errorf("unknown runtime command %q", args[0])
	}
	flags := flag.NewFlagSet("runtime "+args[0], flag.ContinueOnError)
	flags.SetOutput(stderr)
	endpoint := flags.String("endpoint", "http://127.0.0.1:8080", "Control API endpoint")
	pool := flags.String("pool", "", "Runtime pool ID")
	version := flags.String("version", "", "Current pool resource version")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	parsedVersion, err := strconv.ParseInt(*version, 10, 64)
	if strings.TrimSpace(*pool) == "" || err != nil || parsedVersion <= 0 {
		return errors.New("-pool and a positive -version are required")
	}
	reply, err := controlRequestBytes(context.Background(), http.MethodPut, *endpoint,
		"/v1/runtime-pools/"+url.PathEscape(*pool)+"/status",
		map[string]string{"If-Match": fmt.Sprintf(`W/"%d"`, parsedVersion)}, map[string]string{"status": status})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, string(reply))
	return err
}
