package main

import (
	"fmt"
	"os"
	"path"
	"plugin"
	"strings"

	"github.com/Hatch1fy/errors"
	"github.com/Hatch1fy/httpserve"
	"github.com/hatchify/atoms"
	"github.com/vroomy/common"
	"github.com/vroomy/config"
	"github.com/vroomy/plugins"
)

const (
	// ErrInvalidTLSDirectory is returned when a tls directory is unset when the tls port has been set
	ErrInvalidTLSDirectory = errors.Error("invalid tls directory, cannot be empty when tls port has been set")
	// ErrInvalidInitializationFunc is returned when an unsupported initialization function is encountered
	ErrInvalidInitializationFunc = errors.Error("unsupported initialization func encountered")
)

// New will return a new instance of service
func New(cfg *config.Config, dataDir string) (sp *Service, err error) {
	var s Service
	s.cfg = cfg

	if err = os.Chdir(s.cfg.Dir); err != nil {
		err = fmt.Errorf("error changing directory: %v", err)
		return
	}

	if s.plog, err = newPanicLog(); err != nil {
		return
	}

	if err = initDir(dataDir); err != nil {
		err = fmt.Errorf("error initializing data directory: %v", err)
		return
	}

	if err = initDir("build"); err != nil {
		err = fmt.Errorf("error changing plugins directory: %v", err)
		return
	}

	s.srv = httpserve.New()
	if err = s.loadPlugins(); err != nil {
		err = fmt.Errorf("error loading plugins: %v", err)
		return
	}

	if err = s.initPlugins(); err != nil {
		err = fmt.Errorf("error initializing plugins: %v", err)
		return
	}

	if err = s.initGroups(); err != nil {
		err = fmt.Errorf("error initializing groups: %v", err)
		return
	}

	if err = s.initRoutes(); err != nil {
		err = fmt.Errorf("error initializing routes: %v", err)
		return
	}

	sp = &s
	return
}

// Service manages the web service
type Service struct {
	cfg     *config.Config
	srv     *httpserve.Serve
	Plugins *plugins.Plugins

	plog *panicLog
	// Closed state
	closed atoms.Bool
}

func pluginName(key string) (name string) {
	comps := strings.Split(key, " as ")
	if len(comps) > 1 {
		return comps[1]
	}

	key = strings.Split(key, "#")[0]
	key = strings.Split(key, "@")[0]
	_, key = path.Split(key)

	name = key
	return
}

func (s *Service) loadPlugins() (err error) {
	if s.Plugins, err = plugins.New("build"); err != nil {
		err = fmt.Errorf("error initializing plugins manager: %v", err)
		return
	}

	if len(s.cfg.Plugins) == 0 {
		return
	}

	filter, ok := s.cfg.Flags["require"]
	for _, pluginKey := range s.cfg.Plugins {
		if ok && !strings.Contains(filter, pluginName(pluginKey)) {
			continue
		}

		var key string
		if key, err = s.Plugins.New(pluginKey, s.cfg.PerformUpdate); err != nil {
			err = fmt.Errorf("error creating new plugin for key \"%s\": %v", pluginKey, err)
			return
		}

		s.cfg.PluginKeys = append(s.cfg.PluginKeys, key)
	}

	if err = s.Plugins.Initialize(); err != nil {
		err = fmt.Errorf("error initializing plugins: %v", err)
		return
	}

	return
}

func (s *Service) initGroups() (err error) {
	if len(s.cfg.Groups) == 0 {
		return
	}

	filter, ok := s.cfg.Flags["require"]
	for _, group := range s.cfg.Groups {
		if ok {
			var hasPlugin = false
			for _, handler := range group.Handlers {
				if ok && strings.Contains(filter, strings.Split(handler, ".")[0]) {
					hasPlugin = true
					break
				}
			}

			if !hasPlugin {
				continue
			}
		}

		s.initGroup(group)
	}

	return
}

func (s *Service) initGroup(group *config.Group) (err error) {
	if err = group.Init(s.Plugins); err != nil {
		return
	}

	var (
		match *config.Group
		grp   httpserve.Group = s.srv
	)

	if match, err = s.cfg.GetGroup(group.Group); err != nil {
		return
	} else if match != nil {
		grp = match.G
	}

	group.G = grp.Group(group.HTTPPath, group.HttpHandlers...)
	return
}

func (s *Service) initRoutes() (err error) {
	// Set panic func
	s.srv.SetPanic(s.plog.Write)

	filter, ok := s.cfg.Flags["require"]
	for i, r := range s.cfg.Routes {
		if ok {
			var hasPlugin = true
			for _, handler := range r.Handlers {
				if ok && !strings.Contains(filter, strings.Split(handler, ".")[0]) {
					hasPlugin = false
					break
				}
			}

			if !hasPlugin {
				continue
			}
		}

		if err = r.Init(s.Plugins); err != nil {
			return fmt.Errorf("error initializing route #%d (%v): %v", i, r, err)
		}

		var (
			match *config.Group
			grp   httpserve.Group = s.srv
		)

		if match, err = s.cfg.GetGroup(r.Group); err != nil {
			return
		} else if match != nil {
			if match.G == nil {
				s.initGroup(match)
			}

			grp = match.G
		}

		var fn func(string, ...httpserve.Handler)
		switch strings.ToLower(r.Method) {
		case "put":
			fn = grp.PUT
		case "post":
			fn = grp.POST
		case "delete":
			fn = grp.DELETE
		case "options":
			fn = grp.OPTIONS

		default:
			// Default case is GET
			fn = grp.GET
		}

		fn(r.HTTPPath, r.HttpHandlers...)
	}

	return
}

func (s *Service) initPlugins() (err error) {
	for _, pluginKey := range s.cfg.PluginKeys {
		if err = s.initPlugin(pluginKey); err != nil {
			err = fmt.Errorf("error initializing %s: %v", pluginKey, err)
			return
		}
	}

	return
}

func (s *Service) initPlugin(pluginKey string) (err error) {
	var p *plugin.Plugin
	if p, err = s.Plugins.Get(pluginKey); err != nil {
		return
	}

	var sym plugin.Symbol
	if sym, err = p.Lookup("OnInit"); err != nil {
		err = nil
		return
	}

	switch fn := sym.(type) {
	case func(p common.Plugins, flags, env map[string]string) error:
		return fn(s.Plugins, s.cfg.Flags, s.cfg.Environment)
	case func(p common.Plugins, env map[string]string) error:
		return fn(s.Plugins, s.cfg.Environment)

	default:
		return ErrInvalidInitializationFunc
	}
}

func (s *Service) getHTTPListener() (l listener) {
	if s.cfg.TLSPort > 0 {
		// TLS port exists, return a new upgrader pointing to the configured tls port
		return httpserve.NewUpgrader(s.cfg.TLSPort)
	}

	// TLS port does not exist, return the raw httpserve.Serve
	return s.srv
}

func (s *Service) listenHTTP(errC chan error) {
	if s.cfg.Port == 0 {
		// HTTP port not set, return
		return
	}

	// Get http listener
	// Note: If TLS is set, an httpserve.Upgrader will be returned
	l := s.getHTTPListener()

	// Attempt to listen to HTTP with the configured port
	errC <- l.Listen(s.cfg.Port)
}

func (s *Service) listenHTTPS(errC chan error) {
	if s.cfg.TLSPort == 0 {
		// HTTPS port not set, return
		return
	}

	if len(s.cfg.TLSDir) == 0 {
		// Cannot serve TLS without a tls directory, send error down channel and return
		errC <- ErrInvalidTLSDirectory
		return
	}

	// Attempt to listen to HTTPS with the configured tls port and directory
	errC <- s.srv.ListenTLS(s.cfg.TLSPort, s.cfg.TLSDir)
}

// Listen will listen to the configured port
func (s *Service) Listen() (err error) {
	// Initialize error channel
	errC := make(chan error, 2)
	// Listen to HTTP (if needed)
	go s.listenHTTP(errC)
	// Listen to HTTPS (if needed)
	go s.listenHTTPS(errC)
	// Return any error which may come down the error channel
	return <-errC
}

// Port will return the current HTTP port
func (s *Service) Port() uint16 {
	return s.cfg.Port
}

// TLSPort will return the current HTTPS port
func (s *Service) TLSPort() uint16 {
	return s.cfg.TLSPort
}

// Close will close the selected service
func (s *Service) Close() (err error) {
	if !s.closed.Set(true) {
		return errors.ErrIsClosed
	}

	var errs errors.ErrorList
	errs.Push(s.Plugins.Close())
	errs.Push(s.plog.Close())
	return errs.Err()
}