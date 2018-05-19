package main

import (
	"crypto/sha256"
	"crypto/tls"
	"flag"
	"fmt"
	"net"
	"os"
	"runtime"
	"time"

	"github.com/ginuerzh/gost"
	"github.com/go-log/log"
)

var (
	options route
	routes  []route
)

func init() {
	gost.SetLogger(&gost.LogLogger{})

	var (
		configureFile string
		printVersion  bool
	)

	flag.Var(&options.ChainNodes, "F", "forward address, can make a forward chain")
	flag.Var(&options.ServeNodes, "L", "listen address, can listen on multiple ports")
	flag.StringVar(&configureFile, "C", "", "configure file")
	flag.BoolVar(&options.Debug, "D", false, "enable debug log")
	flag.BoolVar(&printVersion, "V", false, "print version")
	flag.Parse()

	if printVersion {
		fmt.Fprintf(os.Stderr, "gost %s (%s)\n", gost.Version, runtime.Version())
		os.Exit(0)
	}

	if len(options.ServeNodes) > 0 {
		routes = append(routes, options)
	}
	gost.Debug = options.Debug

	if err := loadConfigureFile(configureFile); err != nil {
		log.Log(err)
		os.Exit(1)
	}

	if flag.NFlag() == 0 || len(routes) == 0 {
		flag.PrintDefaults()
		os.Exit(0)
	}

}

func main() {
	// NOTE: as of 2.6, you can use custom cert/key files to initialize the default certificate.
	config, err := tlsConfig(defaultCertFile, defaultKeyFile)
	if err != nil {
		// generate random self-signed certificate.
		cert, err := gost.GenCertificate()
		if err != nil {
			log.Log(err)
			os.Exit(1)
		}
		config = &tls.Config{
			Certificates: []tls.Certificate{cert},
		}
	}
	gost.DefaultTLSConfig = config

	for _, route := range routes {
		if err := route.serve(); err != nil {
			log.Log(err)
			os.Exit(1)
		}
	}

	select {}
}

type route struct {
	ChainNodes, ServeNodes stringList
	Retries                int
	Debug                  bool
}

func (r *route) initChain() (*gost.Chain, error) {
	chain := gost.NewChain()

	chain.Retries = r.Retries
	if chain.Retries == 0 {
		chain.Retries = 1
	}

	gid := 1 // group ID

	for _, ns := range r.ChainNodes {
		ngroup := gost.NewNodeGroup()
		ngroup.ID = gid
		gid++

		// parse the base node
		nodes, err := parseChainNode(ns)
		if err != nil {
			return nil, err
		}

		nid := 1 // node ID

		for i := range nodes {
			nodes[i].ID = nid
			nid++
		}
		ngroup.AddNode(nodes...)

		// parse peer nodes if exists
		peerCfg, err := loadPeerConfig(nodes[0].Get("peer"))
		if err != nil {
			log.Log(err)
		}
		peerCfg.Validate()
		ngroup.Options = append(ngroup.Options,
			gost.WithFilter(&gost.FailFilter{
				MaxFails:    peerCfg.MaxFails,
				FailTimeout: time.Duration(peerCfg.FailTimeout) * time.Second,
			}),
			gost.WithStrategy(parseStrategy(peerCfg.Strategy)),
		)

		for _, s := range peerCfg.Nodes {
			nodes, err = parseChainNode(s)
			if err != nil {
				return nil, err
			}

			for i := range nodes {
				nodes[i].ID = nid
				nid++
			}

			ngroup.AddNode(nodes...)
		}

		var bypass *gost.Bypass
		if peerCfg.Bypass != nil {
			bypass = gost.NewBypassPatterns(peerCfg.Bypass.Patterns, peerCfg.Bypass.Reverse)
		}
		nodes = ngroup.Nodes()
		for i := range nodes {
			if nodes[i].Bypass == nil {
				nodes[i].Bypass = bypass // use global bypass if local bypass does not exist.
			}
		}

		chain.AddNodeGroup(ngroup)
	}

	return chain, nil
}

func parseChainNode(ns string) (nodes []gost.Node, err error) {
	node, err := gost.ParseNode(ns)
	if err != nil {
		return
	}

	users, err := parseUsers(node.Get("secrets"))
	if err != nil {
		return
	}
	if node.User == nil && len(users) > 0 {
		node.User = users[0]
	}
	serverName, sport, _ := net.SplitHostPort(node.Addr)
	if serverName == "" {
		serverName = "localhost" // default server name
	}

	rootCAs, err := loadCA(node.Get("ca"))
	if err != nil {
		return
	}
	tlsCfg := &tls.Config{
		ServerName:         serverName,
		InsecureSkipVerify: !node.GetBool("secure"),
		RootCAs:            rootCAs,
	}
	wsOpts := &gost.WSOptions{}
	wsOpts.EnableCompression = node.GetBool("compression")
	wsOpts.ReadBufferSize = node.GetInt("rbuf")
	wsOpts.WriteBufferSize = node.GetInt("wbuf")
	wsOpts.UserAgent = node.Get("agent")

	var tr gost.Transporter
	switch node.Transport {
	case "tls":
		tr = gost.TLSTransporter()
	case "mtls":
		tr = gost.MTLSTransporter()
	case "ws":
		tr = gost.WSTransporter(wsOpts)
	case "mws":
		tr = gost.MWSTransporter(wsOpts)
	case "wss":
		tr = gost.WSSTransporter(wsOpts)
	case "mwss":
		tr = gost.MWSSTransporter(wsOpts)
	case "kcp":
		/*
			if !chain.IsEmpty() {
				return nil, errors.New("KCP must be the first node in the proxy chain")
			}
		*/
		config, err := parseKCPConfig(node.Get("c"))
		if err != nil {
			return nil, err
		}
		tr = gost.KCPTransporter(config)
	case "ssh":
		if node.Protocol == "direct" || node.Protocol == "remote" {
			tr = gost.SSHForwardTransporter()
		} else {
			tr = gost.SSHTunnelTransporter()
		}
	case "quic":
		/*
			if !chain.IsEmpty() {
				return nil, errors.New("QUIC must be the first node in the proxy chain")
			}
		*/
		config := &gost.QUICConfig{
			TLSConfig: tlsCfg,
			KeepAlive: node.GetBool("keepalive"),
		}

		config.Timeout = time.Duration(node.GetInt("timeout")) * time.Second
		config.IdleTimeout = time.Duration(node.GetInt("idle")) * time.Second

		if key := node.Get("key"); key != "" {
			sum := sha256.Sum256([]byte(key))
			config.Key = sum[:]
		}

		tr = gost.QUICTransporter(config)
	case "http2":
		tr = gost.HTTP2Transporter(tlsCfg)
	case "h2":
		tr = gost.H2Transporter(tlsCfg)
	case "h2c":
		tr = gost.H2CTransporter()

	case "obfs4":
		if err := gost.Obfs4Init(node, false); err != nil {
			return nil, err
		}
		tr = gost.Obfs4Transporter()
	case "ohttp":
		tr = gost.ObfsHTTPTransporter()
	default:
		tr = gost.TCPTransporter()
	}

	var connector gost.Connector
	switch node.Protocol {
	case "http2":
		connector = gost.HTTP2Connector(node.User)
	case "socks", "socks5":
		connector = gost.SOCKS5Connector(node.User)
	case "socks4":
		connector = gost.SOCKS4Connector()
	case "socks4a":
		connector = gost.SOCKS4AConnector()
	case "ss":
		connector = gost.ShadowConnector(node.User)
	case "direct":
		connector = gost.SSHDirectForwardConnector()
	case "remote":
		connector = gost.SSHRemoteForwardConnector()
	case "forward":
		connector = gost.ForwardConnector()
	case "sni":
		connector = gost.SNIConnector(node.Get("host"))
	case "http":
		fallthrough
	default:
		node.Protocol = "http" // default protocol is HTTP
		connector = gost.HTTPConnector(node.User)
	}

	timeout := node.GetInt("timeout")
	node.DialOptions = append(node.DialOptions,
		gost.TimeoutDialOption(time.Duration(timeout)*time.Second),
	)

	handshakeOptions := []gost.HandshakeOption{
		gost.AddrHandshakeOption(node.Addr),
		gost.HostHandshakeOption(node.Host),
		gost.UserHandshakeOption(node.User),
		gost.TLSConfigHandshakeOption(tlsCfg),
		gost.IntervalHandshakeOption(time.Duration(node.GetInt("ping")) * time.Second),
		gost.TimeoutHandshakeOption(time.Duration(timeout) * time.Second),
		gost.RetryHandshakeOption(node.GetInt("retry")),
	}
	node.Client = &gost.Client{
		Connector:   connector,
		Transporter: tr,
	}

	node.Bypass = parseBypass(node.Get("bypass"))

	ips := parseIP(node.Get("ip"), sport)
	for _, ip := range ips {
		node.Addr = ip
		// override the default node address
		node.HandshakeOptions = append(handshakeOptions, gost.AddrHandshakeOption(ip))
		nodes = append(nodes, node)
	}
	if len(ips) == 0 {
		node.HandshakeOptions = handshakeOptions
		nodes = []gost.Node{node}
	}

	return
}

func (r *route) serve() error {
	chain, err := r.initChain()
	if err != nil {
		return err
	}

	for _, ns := range r.ServeNodes {
		node, err := gost.ParseNode(ns)
		if err != nil {
			return err
		}
		users, err := parseUsers(node.Get("secrets"))
		if err != nil {
			return err
		}
		if node.User != nil {
			users = append(users, node.User)
		}
		certFile, keyFile := node.Get("cert"), node.Get("key")
		tlsCfg, err := tlsConfig(certFile, keyFile)
		if err != nil && certFile != "" && keyFile != "" {
			return err
		}

		wsOpts := &gost.WSOptions{}
		wsOpts.EnableCompression = node.GetBool("compression")
		wsOpts.ReadBufferSize = node.GetInt("rbuf")
		wsOpts.WriteBufferSize = node.GetInt("wbuf")

		var ln gost.Listener
		switch node.Transport {
		case "tls":
			ln, err = gost.TLSListener(node.Addr, tlsCfg)
		case "mtls":
			ln, err = gost.MTLSListener(node.Addr, tlsCfg)
		case "ws":
			wsOpts.WriteBufferSize = node.GetInt("wbuf")
			ln, err = gost.WSListener(node.Addr, wsOpts)
		case "mws":
			ln, err = gost.MWSListener(node.Addr, wsOpts)
		case "wss":
			ln, err = gost.WSSListener(node.Addr, tlsCfg, wsOpts)
		case "mwss":
			ln, err = gost.MWSSListener(node.Addr, tlsCfg, wsOpts)
		case "kcp":
			config, er := parseKCPConfig(node.Get("c"))
			if er != nil {
				return er
			}
			ln, err = gost.KCPListener(node.Addr, config)
		case "ssh":
			config := &gost.SSHConfig{
				Users:     users,
				TLSConfig: tlsCfg,
			}
			if node.Protocol == "forward" {
				ln, err = gost.TCPListener(node.Addr)
			} else {
				ln, err = gost.SSHTunnelListener(node.Addr, config)
			}
		case "quic":
			config := &gost.QUICConfig{
				TLSConfig: tlsCfg,
				KeepAlive: node.GetBool("keepalive"),
			}
			config.Timeout = time.Duration(node.GetInt("timeout")) * time.Second
			config.IdleTimeout = time.Duration(node.GetInt("idle")) * time.Second

			if key := node.Get("key"); key != "" {
				sum := sha256.Sum256([]byte(key))
				config.Key = sum[:]
			}

			ln, err = gost.QUICListener(node.Addr, config)
		case "http2":
			ln, err = gost.HTTP2Listener(node.Addr, tlsCfg)
		case "h2":
			ln, err = gost.H2Listener(node.Addr, tlsCfg)
		case "h2c":
			ln, err = gost.H2CListener(node.Addr)
		case "tcp":
			// Directly use SSH port forwarding if the last chain node is forward+ssh
			if chain.LastNode().Protocol == "forward" && chain.LastNode().Transport == "ssh" {
				chain.Nodes()[len(chain.Nodes())-1].Client.Connector = gost.SSHDirectForwardConnector()
				chain.Nodes()[len(chain.Nodes())-1].Client.Transporter = gost.SSHForwardTransporter()
			}
			ln, err = gost.TCPListener(node.Addr)
		case "rtcp":
			// Directly use SSH port forwarding if the last chain node is forward+ssh
			if chain.LastNode().Protocol == "forward" && chain.LastNode().Transport == "ssh" {
				chain.Nodes()[len(chain.Nodes())-1].Client.Connector = gost.SSHRemoteForwardConnector()
				chain.Nodes()[len(chain.Nodes())-1].Client.Transporter = gost.SSHForwardTransporter()
			}
			ln, err = gost.TCPRemoteForwardListener(node.Addr, chain)
		case "udp":
			ln, err = gost.UDPDirectForwardListener(node.Addr, time.Duration(node.GetInt("ttl"))*time.Second)
		case "rudp":
			ln, err = gost.UDPRemoteForwardListener(node.Addr, chain, time.Duration(node.GetInt("ttl"))*time.Second)
		case "ssu":
			ln, err = gost.ShadowUDPListener(node.Addr, node.User, time.Duration(node.GetInt("ttl"))*time.Second)
		case "obfs4":
			if err = gost.Obfs4Init(node, true); err != nil {
				return err
			}
			ln, err = gost.Obfs4Listener(node.Addr)
		case "ohttp":
			ln, err = gost.ObfsHTTPListener(node.Addr)
		default:
			ln, err = gost.TCPListener(node.Addr)
		}
		if err != nil {
			return err
		}

		var whitelist, blacklist *gost.Permissions
		if node.Values.Get("whitelist") != "" {
			if whitelist, err = gost.ParsePermissions(node.Values.Get("whitelist")); err != nil {
				return err
			}
		}
		if node.Values.Get("blacklist") != "" {
			if blacklist, err = gost.ParsePermissions(node.Values.Get("blacklist")); err != nil {
				return err
			}
		}

		var handlerOptions []gost.HandlerOption

		handlerOptions = append(handlerOptions,
			gost.AddrHandlerOption(node.Addr),
			gost.ChainHandlerOption(chain),
			gost.UsersHandlerOption(users...),
			gost.TLSConfigHandlerOption(tlsCfg),
			gost.WhitelistHandlerOption(whitelist),
			gost.BlacklistHandlerOption(blacklist),
		)
		var handler gost.Handler
		switch node.Protocol {
		case "http2":
			handler = gost.HTTP2Handler(handlerOptions...)
		case "socks", "socks5":
			handler = gost.SOCKS5Handler(handlerOptions...)
		case "socks4", "socks4a":
			handler = gost.SOCKS4Handler(handlerOptions...)
		case "ss":
			handler = gost.ShadowHandler(handlerOptions...)
		case "http":
			handler = gost.HTTPHandler(handlerOptions...)
		case "tcp":
			handler = gost.TCPDirectForwardHandler(node.Remote, handlerOptions...)
		case "rtcp":
			handler = gost.TCPRemoteForwardHandler(node.Remote, handlerOptions...)
		case "udp":
			handler = gost.UDPDirectForwardHandler(node.Remote, handlerOptions...)
		case "rudp":
			handler = gost.UDPRemoteForwardHandler(node.Remote, handlerOptions...)
		case "forward":
			handler = gost.SSHForwardHandler(handlerOptions...)
		case "redirect":
			handler = gost.TCPRedirectHandler(handlerOptions...)
		case "ssu":
			handler = gost.ShadowUDPdHandler(handlerOptions...)
		case "sni":
			handler = gost.SNIHandler(handlerOptions...)
		default:
			// start from 2.5, if remote is not empty, then we assume that it is a forward tunnel.
			if node.Remote != "" {
				handler = gost.TCPDirectForwardHandler(node.Remote, handlerOptions...)
			} else {
				handler = gost.AutoHandler(handlerOptions...)
			}
		}

		srv := &gost.Server{Listener: ln}
		srv.Init(
			gost.BypassServerOption(parseBypass(node.Get("bypass"))),
		)

		chain.Resolver = parseResolver(node.Get("dns"))

		go srv.Serve(handler)
	}

	return nil
}
