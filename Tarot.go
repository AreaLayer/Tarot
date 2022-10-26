
package main

import (
	_ "embed"
	"fmt"
	"github.com/lightninglabs/taro/chanutils"
	"github.com/lightninglabs/taro/tarocfg"
	"github.com/lightningnetwork/lnd"
	"os"
	"os/exec"
	sig "os/signal"
	"path/filepath"
	"time"
)

func main() {
	var result GoroutineNotifier
	config := LoadConfig()
	fmt.Printf("Tarot starting with config: %+v\n", config)

	// Load LND or timeout and panic
	loadLndConfig := make(chan GoroutineNotifier)
	go Lnd(config, loadLndConfig)
	select {
	case result = <-loadLndConfig:
		if result.err != nil {
			panic(result.err)
		}
	case <-time.After(5 * time.Second):
		fmt.Printf("LND is starting, waiting for macaroon...\n")
	}
	config.LndClient.WaitForMacaroon(time.Minute * 5)
	fmt.Printf("LND RPC Servers are starting, waiting for connection...\n")
	config.LndClient.WaitForConnection(time.Hour * 1)
	for config.LndClient.WaitForSync(time.Minute*1) != nil {
		fmt.Printf("LND is syncing...\n")
	}

	// Load Taro or timeout and panic
	fmt.Printf("Taro is loading...\n")
	loadTaroConfig := make(chan GoroutineNotifier)
	go Taro(config, loadTaroConfig)
	result = <-loadTaroConfig
	if result.err != nil {
		panic(result.err)
	}
	fmt.Printf("Taro loaded.\n")

	// Load terminal or
	fmt.Printf("Terminal loading...\n")
	loadTerminalConfig := make(chan GoroutineNotifier)
	go Terminal(config, loadTerminalConfig)
	result = <-loadTerminalConfig
	if result.err != nil {
		fmt.Printf("Terminal: error loading- %s\n", result.err.Error())
	}
	fmt.Printf("Terminal loaded.\n")

	// Block at interrupt signal
	done := make(chan os.Signal)
	sig.Notify(done, os.Interrupt)
	<-done
}

// Lnd : We pass in commandline arguments because of undefined DefaultConfig unmarshalling behavior in subRPCServers
func Lnd(config Config, loadComplete chan GoroutineNotifier) {
	if err := lnd.Main(
		config.Lnd, lnd.ListenerCfg{}, config.Lnd.ImplementationConfig(config.Interceptor), config.Interceptor,
	); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		loadComplete <- GoroutineNotifier{result: 1, err: err}
		return
	}
}

func Taro(config Config, loadComplete chan GoroutineNotifier) {
	// This concurrent error queue can be used by every component that can
	// raise runtime errors. Using a queue will prevent us from blocking on
	// sending errors to it, as long as the queue is running.
	errQueue := chanutils.NewConcurrentQueue[error](
		chanutils.DefaultQueueSize,
	)
	errQueue.Start()
	defer errQueue.Stop()

	server, err := tarocfg.CreateServerFromConfig(
		config.Taro, config.Logger, config.Interceptor, errQueue.ChanIn(),
	)
	if err != nil {
		err := fmt.Errorf("error creating taro server: %v", err)
		_, _ = fmt.Fprintln(os.Stderr, err)
		loadComplete <- GoroutineNotifier{result: 1, err: err}
		return
	}
	loadComplete <- GoroutineNotifier{result: 0, err: nil}

	err = server.RunUntilShutdown(errQueue.ChanOut())
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		loadComplete <- GoroutineNotifier{result: 1, err: err}
		return
	}
}

// Terminal starts everything and then blocks until either the application is shut
// down or a critical error happens.
func Terminal(config Config, loadComplete chan GoroutineNotifier) {
	ex, err := os.Executable()
	if err != nil {
		panic(err)
	}
	args := []string{
		"--network=" + config.BitcoinNetwork,
		"--enablerest",
		"--lnd-mode=remote",
		"--remote.lnd.rpcserver=" + "127.0.0.1:10009",
		"--remote.lnd.macaroonpath=" + fmt.Sprintf("%s/data/chain/bitcoin/%s/admin.macaroon", config.LnHome, config.BitcoinNetwork),
		"--remote.lnd.tlscertpath=" + fmt.Sprintf("%s/tls.cert", config.LnHome),
		"--lnd.tlskeypath=" + fmt.Sprintf("%s/tls.key", config.LnHome),
		"--uipassword=" + config.UIPassword,
	}
	exPath := filepath.Dir(ex)
	cmd := exec.Command(fmt.Sprintf("%s/litd", exPath), args...)
	stdout, err := cmd.StdoutPipe()
	cmd.Stderr = cmd.Stdout
	if err != nil {
		loadComplete <- GoroutineNotifier{result: 1, err: err}
	}
	if err = cmd.Start(); err != nil {
		loadComplete <- GoroutineNotifier{result: 1, err: err}
	}
	go func() {
		for {
			tmp := make([]byte, 1024)
			_, err := stdout.Read(tmp)
			fmt.Print(string(tmp))
			if err != nil {
				break
			}
		}
	}()
	loadComplete <- GoroutineNotifier{result: 0, err: nil}
}
