package main

import (
  "fmt"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"strings"
	"time"

  "github.com/armed/mkdirp"
	"github.com/armon/consul-api"
)

// Configuration for watches.
type WatchConfig struct {
	ConsulAddr string
	ConsulDC   string
	OnChange   []string
	Prefix     string
	Path       string
}

var (
	// Regexp for invalid characters in keys
	InvalidRegexp = regexp.MustCompile(`[^a-zA-Z0-9_]`)
)

// Connects to Consul and watches a given K/V prefix and uses that to
// write to the filesystem.
func watchAndExec(config *WatchConfig) (int, error) {
	kvConfig := consulapi.DefaultConfig()
	kvConfig.Address = config.ConsulAddr
	kvConfig.Datacenter = config.ConsulDC

	client, err := consulapi.NewClient(kvConfig)
	if err != nil {
		return 0, err
	}

	// Start the watcher goroutine that watches for changes in the
	// K/V and notifies us on a channel.
	errCh := make(chan error, 1)
	pairCh := make(chan consulapi.KVPairs)
	quitCh := make(chan struct{})
	defer close(quitCh)
	go watch(
		client, config.Prefix, config.Path, pairCh, errCh, quitCh)

	var env map[string]string
	for {
		var pairs consulapi.KVPairs

		// Wait for new pairs to come on our channel or an error
		// to occur.
		select {
		case pairs = <-pairCh:
		case err := <-errCh:
			return 0, err
		}

		newEnv := make(map[string]string)
		for _, pair := range pairs {
			k := strings.TrimPrefix(pair.Key, config.Prefix)
			k = strings.TrimLeft(k, "/")
			newEnv[k] = string(pair.Value)
		}

		// If the variables didn't actually change,
		// then don't do anything.
		if reflect.DeepEqual(env, newEnv) {
			continue
		}

		// Replace the env so we can detect future changes
		env = newEnv

    fmt.Println(newEnv)

    // Write the updated keys to the filesystem at the specified path
		for k, v := range newEnv {
      // Write file to disk
      fmt.Printf("%s=%s\n", k, v)
      
      // TODO: Add OS-appropriate delimiter to config.Path if not present
      keyfile := fmt.Sprintf("%s%s", config.Path, k)
      
      // TODO: Scream bloody murder if this fails
      // mkdirp the file's path
      mkdirp.Mk(keyfile[:strings.LastIndex(keyfile, "/")], 0777)
      
      f, err := os.Create(keyfile)
      if err != nil {
        fmt.Printf("Failed to create file %s due to %s\n", keyfile, err)
        continue
      }
      
      defer f.Close()
      
      wrote, err := f.WriteString(v)
      if err != nil {
        fmt.Printf("Failed to write value %s to file %s due to %s\n", v, keyfile, err)
        continue
      }
      
      fmt.Printf("Successfully wrote %d bytes to %s\n", wrote, keyfile)
      
      f.Sync()
    }

		// Configuration changed, run our onchange command.
		var cmd = exec.Command(config.OnChange[0], config.OnChange[1:]...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		err := cmd.Start()
		if err != nil {
			return 111, err
		}
	}

	return 0, nil
}

func watch(
	client *consulapi.Client,
	prefix string,
	path string,
	pairCh chan<- consulapi.KVPairs,
	errCh chan<- error,
	quitCh <-chan struct{}) {

  // Create the root for KVs, if necessary
  mkdirp.Mk(path, 0777)

	// Get the initial list of k/v pairs. We don't do a retryableList
	// here because we want a fast fail if the initial request fails.
	pairs, meta, err := client.KV().List(prefix, nil)
	if err != nil {
		errCh <- err
		return
	}

	// Send the initial list out right away
	pairCh <- pairs

	// Loop forever (or until quitCh is closed) and watch the keys
	// for changes.
	curIndex := meta.LastIndex
	for {
		select {
		case <-quitCh:
			return
		default:
		}

		pairs, meta, err = retryableList(
			func() (consulapi.KVPairs, *consulapi.QueryMeta, error) {
				opts := &consulapi.QueryOptions{WaitIndex: curIndex}
				return client.KV().List(prefix, opts)
			})

		pairCh <- pairs
		curIndex = meta.LastIndex
	}
}

// This function is able to call KV listing functions and retry them.
// We want to retry if there are errors because it is safe (GET request),
// and erroring early is MUCH more costly than retrying over time and
// delaying the configuration propagation.
func retryableList(f func() (consulapi.KVPairs, *consulapi.QueryMeta, error)) (consulapi.KVPairs, *consulapi.QueryMeta, error) {
	i := 0
	for {
		p, m, e := f()
		if e != nil {
			if i >= 3 {
				return nil, nil, e
			}

			i++

			// Reasonably arbitrary sleep to just try again... It is
			// a GET request so this is safe.
			time.Sleep(time.Duration(i*2) * time.Second)
		}

		return p, m, e
	}
}