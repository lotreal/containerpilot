package control

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/joyent/containerpilot/events"
)

// SocketType is the default listener type
var SocketType = "unix"

// HTTPServer contains the state of the HTTP Server used by ContainerPilot's
// HTTP transport control plane. Currently this is listening via a UNIX socket
// file.
type HTTPServer struct {
	http.Server
	Addr                string
	events.EventHandler // Event handling
}

// NewHTTPServer initializes a new control server for manipulating
// ContainerPilot's runtime configuration.
func NewHTTPServer(cfg *Config) (*HTTPServer, error) {
	if cfg == nil {
		err := errors.New("control server not loading due to missing config")
		return nil, err
	}
	srv := &HTTPServer{
		Addr: cfg.SocketPath,
	}
	srv.Rx = make(chan events.Event, 10)
	return srv, nil
}

// Run executes the event loop for the control server
func (srv *HTTPServer) Run(bus *events.EventBus) {
	srv.Subscribe(bus, true)
	srv.Bus = bus
	srv.Start()

	go func() {
		defer srv.Stop()
		for {
			event := <-srv.Rx
			switch event {
			case
				events.QuitByClose,
				events.GlobalShutdown:
				return
			}
		}
	}()
}

// Start sets up API routes with the event bus, listens on the control
// socket, and serves the HTTP server.
func (srv *HTTPServer) Start() {
	endpoints := &Endpoints{srv.Bus}

	router := http.NewServeMux()
	router.Handle("/v3/environ", PostHandler(endpoints.PutEnviron))
	router.Handle("/v3/reload", PostHandler(endpoints.PostReload))
	router.Handle("/v3/metric", PostHandler(endpoints.PostMetric))
	router.Handle("/v3/maintenance/enable",
		PostHandler(endpoints.PostEnableMaintenanceMode))
	router.Handle("/v3/maintenance/disable",
		PostHandler(endpoints.PostDisableMaintenanceMode))

	srv.Handler = router
	srv.SetKeepAlivesEnabled(false)
	log.Debug("control: initialized router for control server")

	ln := srv.listenWithRetry()

	go func() {
		log.Infof("control: serving at %s", srv.Addr)
		srv.Serve(ln)
		log.Debugf("control: stopped serving at %s", srv.Addr)
	}()

}

// on a reload we can't guarantee that the control server will be shut down
// and the socket file cleaned up before we're ready to start again, so we'll
// retry with the listener a few times before bailing out.
func (srv *HTTPServer) listenWithRetry() net.Listener {
	var (
		err error
		ln  net.Listener
	)
	for i := 0; i < 10; i++ {
		ln, err = net.Listen(SocketType, srv.Addr)
		if err == nil {
			return ln
		}
		time.Sleep(time.Second)
	}
	log.Fatalf("error listening to socket at %s: %v", srv.Addr, err)
	return nil
}

// Stop shuts down the control server gracefully
func (srv *HTTPServer) Stop() error {
	// This timeout won't stop the configuration reload process, since that
	// happens async, but timing out can pre-emptively close the HTTP connection
	// that fired the reload in the first place. If pre-emptive timeout occurs
	// than CP only throws a warning in its logs.
	//
	// Also, 600 seemed to be the magic number... I'm sure it'll vary.
	log.Debug("control: stopping control server")
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	defer os.Remove(srv.Addr)
	if err := srv.Shutdown(ctx); err != nil {
		log.Warnf("control: failed to gracefully shutdown control server: %v", err)
		return err
	}

	srv.Unsubscribe(srv.Bus, true)
	close(srv.Rx)
	log.Debug("control: completed graceful shutdown of control server")
	return nil
}
