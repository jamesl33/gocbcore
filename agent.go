// Package gocbcore implements methods for low-level communication
// with a Couchbase Server cluster.
package gocbcore

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/couchbaselabs/gocbconnstr"
	"golang.org/x/net/http2"
)

// Agent represents the base client handling connections to a Couchbase Server.
// This is used internally by the higher level classes for communicating with the cluster,
// it can also be used to perform more advanced operations with a cluster.
type Agent struct {
	clientId             string
	userString           string
	auth                 AuthProvider
	authHandler          AuthFunc
	bucketName           string
	bucketLock           sync.Mutex
	tlsConfig            *tls.Config
	initFn               memdInitFunc
	networkType          string
	useMutationTokens    bool
	useKvErrorMaps       bool
	useEnhancedErrors    bool
	useCompression       bool
	useDurations         bool
	disableDecompression bool
	useCollections       bool

	compressionMinSize  int
	compressionMinRatio float64

	closeNotify        chan struct{}
	cccpLooperDoneSig  chan struct{}
	httpLooperDoneSig  chan struct{}
	gcccpLooperDoneSig chan struct{}
	gcccpLooperStopSig chan struct{}

	configLock  sync.Mutex
	routingInfo routeDataPtr
	kvErrorMap  kvErrorMapPtr
	numVbuckets int

	serverFailuresLock sync.Mutex
	serverFailures     map[string]time.Time

	httpCli *http.Client

	confHttpRedialPeriod time.Duration
	confHttpRetryDelay   time.Duration
	confCccpMaxWait      time.Duration
	confCccpPollPeriod   time.Duration

	serverConnectTimeout time.Duration
	serverWaitTimeout    time.Duration
	nmvRetryDelay        time.Duration
	kvPoolSize           int
	maxQueueSize         int

	zombieLock      sync.RWMutex
	zombieOps       []*zombieLogEntry
	useZombieLogger bool

	dcpPriority  DcpAgentPriority
	useDcpExpiry bool

	cidMgr *collectionIdManager

	durabilityLevelStatus durabilityLevelStatus
	clusterCapabilities   uint32

	cachedClients       map[string]*memdClient
	cachedClientsLock   sync.Mutex
	cachedHTTPEndpoints []string
	supportsGCCCP       bool
}

// ServerConnectTimeout gets the timeout for each server connection, including all authentication steps.
func (agent *Agent) ServerConnectTimeout() time.Duration {
	return agent.serverConnectTimeout
}

// SetServerConnectTimeout sets the timeout for each server connection.
func (agent *Agent) SetServerConnectTimeout(timeout time.Duration) {
	agent.serverConnectTimeout = timeout
}

// HttpClient returns a pre-configured HTTP Client for communicating with
// Couchbase Server.  You must still specify authentication information
// for any dispatched requests.
func (agent *Agent) HttpClient() *http.Client {
	return agent.httpCli
}

func (agent *Agent) getErrorMap() *kvErrorMap {
	return agent.kvErrorMap.Get()
}

// AuthFunc is invoked by the agent to authenticate a client. This function returns two channels to allow for for multi-stage
// authentication processes (such as SCRAM). The continue channel should be called when further asynchronous bootstrapping
// requests (such as select bucket) can be considered, if false is sent on the channel then the further requests will not
// be sent. The completed channel should be called when authentication is completed, containing any error that occurred.
type AuthFunc func(client AuthClient, deadline time.Time) (completedCh chan BytesAndError, continueCh chan bool, err error)

// AgentConfig specifies the configuration options for creation of an Agent.
type AgentConfig struct {
	UserString     string
	MemdAddrs      []string
	HttpAddrs      []string
	TlsConfig      *tls.Config
	BucketName     string
	NetworkType    string
	AuthHandler    AuthFunc
	Auth           AuthProvider
	AuthMechanisms []AuthMechanism

	UseMutationTokens    bool
	UseKvErrorMaps       bool
	UseEnhancedErrors    bool
	UseCompression       bool
	UseDurations         bool
	DisableDecompression bool
	UseCollections       bool

	CompressionMinSize  int
	CompressionMinRatio float64

	HttpRedialPeriod time.Duration
	HttpRetryDelay   time.Duration
	CccpMaxWait      time.Duration
	CccpPollPeriod   time.Duration

	ConnectTimeout       time.Duration
	ServerConnectTimeout time.Duration
	NmvRetryDelay        time.Duration
	KvPoolSize           int
	MaxQueueSize         int

	HttpMaxIdleConns        int
	HttpMaxIdleConnsPerHost int
	HttpIdleConnTimeout     time.Duration

	UseZombieLogger        bool
	ZombieLoggerInterval   time.Duration
	ZombieLoggerSampleSize int

	DcpAgentPriority DcpAgentPriority
	UseDcpExpiry     bool

	EnableStreamId bool
}

// FromConnStr populates the AgentConfig with information from a
// Couchbase Connection String.
// Supported options are:
//   cacertpath (string) - Path to the CA certificate
//   certpath (string) - Path to your authentication certificate
//   keypath (string) - Path to your authentication key
//   config_total_timeout (int) - Maximum period to attempt to connect to cluster in ms.
//   config_node_timeout (int) - Maximum period to attempt to connect to a node in ms.
//   http_redial_period (int) - Maximum period to keep HTTP config connections open in ms.
//   http_retry_delay (int) - Period to wait between retrying nodes for HTTP config in ms.
//   config_poll_floor_interval (int) - Minimum time to wait between fetching configs via CCCP in ms.
//   config_poll_interval (int) - Period to wait between CCCP config polling in ms.
//   kv_pool_size (int) - The number of connections to establish per node.
//   max_queue_size (int) - The maximum size of the operation queues per node.
//   use_kverrmaps (bool) - Whether to enable error maps from the server.
//   use_enhanced_errors (bool) - Whether to enable enhanced error information.
//   fetch_mutation_tokens (bool) - Whether to fetch mutation tokens for operations.
//   compression (bool) - Whether to enable network-wise compression of documents.
//   compression_min_size (int) - The minimal size of the document to consider compression.
//   compression_min_ratio (float64) - The minimal compress ratio (compressed / original) for the document to be sent compressed.
//   server_duration (bool) - Whether to enable fetching server operation durations.
//   http_max_idle_conns (int) - Maximum number of idle http connections in the pool.
//   http_max_idle_conns_per_host (int) - Maximum number of idle http connections in the pool per host.
//   http_idle_conn_timeout (int) - Maximum length of time for an idle connection to stay in the pool in ms.
//   network (string) - The network type to use
func (config *AgentConfig) FromConnStr(connStr string) error {
	baseSpec, err := gocbconnstr.Parse(connStr)
	if err != nil {
		return err
	}

	spec, err := gocbconnstr.Resolve(baseSpec)
	if err != nil {
		return err
	}

	fetchOption := func(name string) (string, bool) {
		optValue := spec.Options[name]
		if len(optValue) == 0 {
			return "", false
		}
		return optValue[len(optValue)-1], true
	}

	// Grab the resolved hostnames into a set of string arrays
	var httpHosts []string
	for _, specHost := range spec.HttpHosts {
		httpHosts = append(httpHosts, fmt.Sprintf("%s:%d", specHost.Host, specHost.Port))
	}

	var memdHosts []string
	for _, specHost := range spec.MemdHosts {
		memdHosts = append(memdHosts, fmt.Sprintf("%s:%d", specHost.Host, specHost.Port))
	}

	// Get bootstrap_on option to determine which, if any, of the bootstrap nodes should be cleared
	switch val, _ := fetchOption("bootstrap_on"); val {
	case "http":
		memdHosts = nil
		if len(httpHosts) == 0 {
			return errors.New("bootstrap_on=http but no HTTP hosts in connection string")
		}
	case "cccp":
		httpHosts = nil
		if len(memdHosts) == 0 {
			return errors.New("bootstrap_on=cccp but no CCCP/Memcached hosts in connection string")
		}
	case "both":
	case "":
		// Do nothing
		break
	default:
		return errors.New("bootstrap_on={http,cccp,both}")
	}
	config.MemdAddrs = memdHosts
	config.HttpAddrs = httpHosts

	var tlsConfig *tls.Config
	if spec.UseSsl {
		var certpath string
		var keypath string
		var cacertpaths []string

		if len(spec.Options["cacertpath"]) > 0 || len(spec.Options["keypath"]) > 0 {
			cacertpaths = spec.Options["cacertpath"]
			certpath, _ = fetchOption("certpath")
			keypath, _ = fetchOption("keypath")
		} else {
			cacertpaths = spec.Options["certpath"]
		}

		tlsConfig = &tls.Config{}

		if len(cacertpaths) > 0 {
			roots := x509.NewCertPool()

			for _, path := range cacertpaths {
				cacert, err := ioutil.ReadFile(path)
				if err != nil {
					return err
				}

				ok := roots.AppendCertsFromPEM(cacert)
				if !ok {
					return ErrInvalidCert
				}
			}

			tlsConfig.RootCAs = roots
		} else {
			tlsConfig.InsecureSkipVerify = true
		}

		if certpath != "" && keypath != "" {
			cert, err := tls.LoadX509KeyPair(certpath, keypath)
			if err != nil {
				return err
			}

			tlsConfig.Certificates = []tls.Certificate{cert}
		}
	}
	config.TlsConfig = tlsConfig

	if spec.Bucket != "" {
		config.BucketName = spec.Bucket
	}

	if valStr, ok := fetchOption("config_total_timeout"); ok {
		val, err := strconv.ParseInt(valStr, 10, 64)
		if err != nil {
			return fmt.Errorf("config_total_timeout option must be a number")
		}
		config.ConnectTimeout = time.Duration(val) * time.Millisecond
	}

	if valStr, ok := fetchOption("config_node_timeout"); ok {
		val, err := strconv.ParseInt(valStr, 10, 64)
		if err != nil {
			return fmt.Errorf("config_node_timeout option must be a number")
		}
		config.ServerConnectTimeout = time.Duration(val) * time.Millisecond
	}

	// This option is experimental
	if valStr, ok := fetchOption("http_redial_period"); ok {
		val, err := strconv.ParseInt(valStr, 10, 64)
		if err != nil {
			return fmt.Errorf("http redial period option must be a number")
		}
		config.HttpRedialPeriod = time.Duration(val) * time.Millisecond
	}

	// This option is experimental
	if valStr, ok := fetchOption("http_retry_delay"); ok {
		val, err := strconv.ParseInt(valStr, 10, 64)
		if err != nil {
			return fmt.Errorf("http retry delay option must be a number")
		}
		config.HttpRetryDelay = time.Duration(val) * time.Millisecond
	}

	if valStr, ok := fetchOption("config_poll_floor_interval"); ok {
		val, err := strconv.ParseInt(valStr, 10, 64)
		if err != nil {
			return fmt.Errorf("config pool floor interval option must be a number")
		}
		config.CccpMaxWait = time.Duration(val) * time.Millisecond
	}

	if valStr, ok := fetchOption("config_poll_interval"); ok {
		val, err := strconv.ParseInt(valStr, 10, 64)
		if err != nil {
			return fmt.Errorf("config pool interval option must be a number")
		}
		config.CccpPollPeriod = time.Duration(val) * time.Millisecond
	}

	// This option is experimental
	if valStr, ok := fetchOption("kv_pool_size"); ok {
		val, err := strconv.ParseInt(valStr, 10, 64)
		if err != nil {
			return fmt.Errorf("kv pool size option must be a number")
		}
		config.KvPoolSize = int(val)
	}

	// This option is experimental
	if valStr, ok := fetchOption("max_queue_size"); ok {
		val, err := strconv.ParseInt(valStr, 10, 64)
		if err != nil {
			return fmt.Errorf("max queue size option must be a number")
		}
		config.MaxQueueSize = int(val)
	}

	if valStr, ok := fetchOption("use_kverrmaps"); ok {
		val, err := strconv.ParseBool(valStr)
		if err != nil {
			return fmt.Errorf("use_kverrmaps option must be a boolean")
		}
		config.UseKvErrorMaps = val
	}

	if valStr, ok := fetchOption("use_enhanced_errors"); ok {
		val, err := strconv.ParseBool(valStr)
		if err != nil {
			return fmt.Errorf("use_enhanced_errors option must be a boolean")
		}
		config.UseEnhancedErrors = val
	}

	if valStr, ok := fetchOption("fetch_mutation_tokens"); ok {
		val, err := strconv.ParseBool(valStr)
		if err != nil {
			return fmt.Errorf("fetch_mutation_tokens option must be a boolean")
		}
		config.UseMutationTokens = val
	}

	if valStr, ok := fetchOption("compression"); ok {
		val, err := strconv.ParseBool(valStr)
		if err != nil {
			return fmt.Errorf("compression option must be a boolean")
		}
		config.UseCompression = val
	}

	if valStr, ok := fetchOption("compression_min_size"); ok {
		val, err := strconv.ParseInt(valStr, 10, 64)
		if err != nil {
			return fmt.Errorf("compression_min_size option must be an int")
		}
		config.CompressionMinSize = int(val)
	}

	if valStr, ok := fetchOption("compression_min_ratio"); ok {
		val, err := strconv.ParseFloat(valStr, 64)
		if err != nil {
			return fmt.Errorf("compression_min_size option must be an int")
		}
		config.CompressionMinRatio = val
	}

	if valStr, ok := fetchOption("server_duration"); ok {
		val, err := strconv.ParseBool(valStr)
		if err != nil {
			return fmt.Errorf("server_duration option must be a boolean")
		}
		config.UseDurations = val
	}

	if valStr, ok := fetchOption("http_max_idle_conns"); ok {
		val, err := strconv.ParseInt(valStr, 10, 64)
		if err != nil {
			return fmt.Errorf("http max idle connections option must be a number")
		}
		config.HttpMaxIdleConns = int(val)
	}

	if valStr, ok := fetchOption("http_max_idle_conns_per_host"); ok {
		val, err := strconv.ParseInt(valStr, 10, 64)
		if err != nil {
			return fmt.Errorf("http max idle connections per host option must be a number")
		}
		config.HttpMaxIdleConnsPerHost = int(val)
	}

	if valStr, ok := fetchOption("http_idle_conn_timeout"); ok {
		val, err := strconv.ParseInt(valStr, 10, 64)
		if err != nil {
			return fmt.Errorf("http idle connection timeout option must be a number")
		}
		config.HttpIdleConnTimeout = time.Duration(val) * time.Millisecond
	}

	if valStr, ok := fetchOption("orphaned_response_logging"); ok {
		val, err := strconv.ParseBool(valStr)
		if err != nil {
			return fmt.Errorf("orphaned_response_logging option must be a boolean")
		}
		config.UseZombieLogger = val
	}

	if valStr, ok := fetchOption("orphaned_response_logging_interval"); ok {
		val, err := strconv.ParseInt(valStr, 10, 64)
		if err != nil {
			return fmt.Errorf("orphaned_response_logging_interval option must be a number")
		}
		config.ZombieLoggerInterval = time.Duration(val) * time.Millisecond
	}

	if valStr, ok := fetchOption("orphaned_response_logging_sample_size"); ok {
		val, err := strconv.ParseInt(valStr, 10, 64)
		if err != nil {
			return fmt.Errorf("orphaned_response_logging_sample_size option must be a number")
		}
		config.ZombieLoggerSampleSize = int(val)
	}

	if valStr, ok := fetchOption("network"); ok {
		if valStr == "default" {
			valStr = ""
		}

		config.NetworkType = valStr
	}

	if valStr, ok := fetchOption("dcp_priority"); ok {
		var priority DcpAgentPriority
		switch valStr {
		case "":
			priority = DcpAgentPriorityLow
		case "low":
			priority = DcpAgentPriorityLow
		case "medium":
			priority = DcpAgentPriorityMed
		case "high":
			priority = DcpAgentPriorityHigh
		default:
			return fmt.Errorf("dcp_priority must be one of low, medium or high")
		}
		config.DcpAgentPriority = priority
	}

	if valStr, ok := fetchOption("enable_expiry_opcode"); ok {
		val, err := strconv.ParseBool(valStr)
		if err != nil {
			return fmt.Errorf("enable_expiry_opcode option must be a boolean")
		}
		config.UseDcpExpiry = val
	}

	return nil
}

// CreateAgent creates an agent for performing normal operations.
// This will create a new agent and attempt to connect it to the cluster,
// if connecting fails (for reasons other than auth or invalid bucket) then
// this function will NOT return an error and will instead continue to
// retry the connection asynchronously. The PingKvEx command can be used
// verify if the connection was successful.
func CreateAgent(config *AgentConfig) (*Agent, error) {
	initFn := func(client *syncClient, deadline time.Time, agent *Agent) error {
		return nil
	}

	return createAgent(config, initFn)
}

// CreateDcpAgent creates an agent for performing DCP operations.
// This will create a new agent and attempt to connect it to the cluster,
// if connecting fails (for reasons other than auth or invalid bucket) then
// this function will NOT return an error and will instead continue to
// retry the connection asynchronously. The PingKvEx command can be used
// verify if the connection was successful.
func CreateDcpAgent(config *AgentConfig, dcpStreamName string, openFlags DcpOpenFlag) (*Agent, error) {
	// We wrap the authorization system to force DCP channel opening
	//   as part of the "initialization" for any servers.
	initFn := func(client *syncClient, deadline time.Time, agent *Agent) error {
		if err := client.ExecOpenDcpConsumer(dcpStreamName, openFlags, deadline); err != nil {
			return err
		}
		if err := client.ExecEnableDcpNoop(180*time.Second, deadline); err != nil {
			return err
		}
		var priority string
		switch agent.dcpPriority {
		case DcpAgentPriorityLow:
			priority = "low"
		case DcpAgentPriorityMed:
			priority = "medium"
		case DcpAgentPriorityHigh:
			priority = "high"
		}
		if err := client.ExecDcpControl("set_priority", priority, deadline); err != nil {
			return err
		}

		if agent.useDcpExpiry {
			if err := client.ExecDcpControl("enable_expiry_opcode", "true", deadline); err != nil {
				return err
			}
		}

		if config.EnableStreamId {
			if err := client.ExecDcpControl("enable_stream_id", "true", deadline); err != nil {
				return err
			}
		}

		if err := client.ExecEnableDcpClientEnd(deadline); err != nil {
			return err
		}
		return client.ExecEnableDcpBufferAck(8*1024*1024, deadline)
	}

	return createAgent(config, initFn)
}

func createAgent(config *AgentConfig, initFn memdInitFunc) (*Agent, error) {
	// TODO(brett19): Put all configurable options in the AgentConfig

	logDebugf("SDK Version: gocb/%s", goCbCoreVersionStr)
	logDebugf("Creating new agent: %+v", config)

	httpTransport := &http.Transport{
		TLSClientConfig: config.TlsConfig,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
		MaxIdleConns:        config.HttpMaxIdleConns,
		MaxIdleConnsPerHost: config.HttpMaxIdleConnsPerHost,
		IdleConnTimeout:     config.HttpIdleConnTimeout,
	}
	err := http2.ConfigureTransport(httpTransport)
	if err != nil {
		logDebugf("failed to configure http2: %s", err)
	}

	maxQueueSize := 2048

	c := &Agent{
		clientId:    formatCbUid(randomCbUid()),
		userString:  config.UserString,
		bucketName:  config.BucketName,
		auth:        config.Auth,
		authHandler: config.AuthHandler,
		tlsConfig:   config.TlsConfig,
		initFn:      initFn,
		networkType: config.NetworkType,
		httpCli: &http.Client{
			Transport: httpTransport,
		},
		closeNotify:           make(chan struct{}),
		useZombieLogger:       config.UseZombieLogger,
		useMutationTokens:     config.UseMutationTokens,
		useKvErrorMaps:        config.UseKvErrorMaps,
		useEnhancedErrors:     config.UseEnhancedErrors,
		useCompression:        config.UseCompression,
		compressionMinSize:    32,
		compressionMinRatio:   0.83,
		useDurations:          config.UseDurations,
		useCollections:        config.UseCollections,
		serverFailures:        make(map[string]time.Time),
		serverConnectTimeout:  7000 * time.Millisecond,
		serverWaitTimeout:     5 * time.Second,
		nmvRetryDelay:         100 * time.Millisecond,
		kvPoolSize:            1,
		maxQueueSize:          maxQueueSize,
		confHttpRetryDelay:    10 * time.Second,
		confHttpRedialPeriod:  10 * time.Second,
		confCccpMaxWait:       3 * time.Second,
		confCccpPollPeriod:    2500 * time.Millisecond,
		dcpPriority:           config.DcpAgentPriority,
		disableDecompression:  config.DisableDecompression,
		useDcpExpiry:          config.UseDcpExpiry,
		durabilityLevelStatus: durabilityLevelStatusUnknown,
		cachedClients:         make(map[string]*memdClient),
	}
	c.cidMgr = newCollectionIdManager(c, maxQueueSize)

	connectTimeout := 60000 * time.Millisecond
	if config.ConnectTimeout > 0 {
		connectTimeout = config.ConnectTimeout
	}

	if config.ServerConnectTimeout > 0 {
		c.serverConnectTimeout = config.ServerConnectTimeout
	}
	if config.NmvRetryDelay > 0 {
		c.nmvRetryDelay = config.NmvRetryDelay
	}
	if config.KvPoolSize > 0 {
		c.kvPoolSize = config.KvPoolSize
	}
	if config.MaxQueueSize > 0 {
		c.maxQueueSize = config.MaxQueueSize
	}
	if config.HttpRetryDelay > 0 {
		c.confHttpRetryDelay = config.HttpRetryDelay
	}
	if config.HttpRedialPeriod > 0 {
		c.confHttpRedialPeriod = config.HttpRedialPeriod
	}
	if config.CccpMaxWait > 0 {
		c.confCccpMaxWait = config.CccpMaxWait
	}
	if config.CccpPollPeriod > 0 {
		c.confCccpPollPeriod = config.CccpPollPeriod
	}
	if config.CompressionMinSize > 0 {
		c.compressionMinSize = config.CompressionMinSize
	}
	if config.CompressionMinRatio > 0 {
		c.compressionMinRatio = config.CompressionMinRatio
		if c.compressionMinRatio >= 1.0 {
			c.compressionMinRatio = 1.0
		}
	}

	deadline := time.Now().Add(connectTimeout)
	if config.BucketName == "" {
		if err := c.connectG3CP(config.MemdAddrs, config.HttpAddrs, config.AuthMechanisms, deadline); err != nil {
			return nil, err
		}
	} else {
		if err := c.connectWithBucket(config.MemdAddrs, config.HttpAddrs, config.AuthMechanisms, deadline); err != nil {
			return nil, err
		}
	}

	if config.UseZombieLogger {
		zombieLoggerInterval := 10 * time.Second
		zombieLoggerSampleSize := 10
		if config.ZombieLoggerInterval > 0 {
			zombieLoggerInterval = config.ZombieLoggerInterval
		}
		if config.ZombieLoggerSampleSize > 0 {
			zombieLoggerSampleSize = config.ZombieLoggerSampleSize
		}
		// zombieOps must have a static capacity for its lifetime, the capacity should
		// never be altered so that it is consistent across the zombieLogger and
		// recordZombieResponse.
		c.zombieOps = make([]*zombieLogEntry, 0, zombieLoggerSampleSize)
		go c.zombieLogger(zombieLoggerInterval, zombieLoggerSampleSize)
	}

	return c, nil
}

func (agent *Agent) buildAuthHandler(client AuthClient, authMechanisms []AuthMechanism,
	deadline time.Time) (func(mechanism AuthMechanism), error) {

	if len(authMechanisms) == 0 {
		// If we're using something like client auth then we might not want an auth handler.
		return nil, nil
	}

	var nextAuth func(mechanism AuthMechanism)
	creds, err := getKvAuthCreds(agent.auth, client.Address())
	if err != nil {
		return nil, err
	}

	if creds.Username != "" || creds.Password != "" {
		// If we only have 1 auth mechanism then we've either we've already decided what mechanism to use
		// or the user has only decided to support 1. Either way we don't need to check what the server supports.
		getAuthFunc := func(mechanism AuthMechanism, deadline time.Time) AuthFunc {
			return func(client AuthClient, deadline time.Time) (completedCh chan BytesAndError, continueCh chan bool, err error) {
				return saslMethod(mechanism, creds.Username, creds.Password, client, deadline)
			}
		}

		if len(authMechanisms) == 1 {
			agent.authHandler = getAuthFunc(authMechanisms[0], deadline)
		} else {
			nextAuth = func(mechanism AuthMechanism) {
				agent.authHandler = getAuthFunc(mechanism, deadline)
			}
			agent.authHandler = getAuthFunc(authMechanisms[0], deadline)
		}
	}

	return nextAuth, nil
}

func (agent *Agent) connectWithBucket(memdAddrs, httpAddrs []string, authMechanisms []AuthMechanism, deadline time.Time) error {
	cccpUnsupported := false
	for _, thisHostPort := range memdAddrs {
		logDebugf("Trying server at %s for %p", thisHostPort, agent)

		srvDeadlineTm := time.Now().Add(agent.serverConnectTimeout)
		if srvDeadlineTm.After(deadline) {
			srvDeadlineTm = deadline
		}

		logDebugf("Trying to connect %p/%s", agent, thisHostPort)
		client, err := agent.dialMemdClient(thisHostPort, srvDeadlineTm)
		if err != nil {
			logDebugf("Connecting failed %p/%s! %v", agent, thisHostPort, err)
			continue
		}

		syncCli := syncClient{
			client: client,
		}

		var nextAuth func(mechanism AuthMechanism)
		if agent.authHandler == nil {
			nextAuth, err = agent.buildAuthHandler(&syncCli, authMechanisms, srvDeadlineTm)
			if err != nil {
				logDebugf("Building auth failed %p/%s! %v", agent, thisHostPort, err)
				continue
			}
		}

		logDebugf("Trying to bootstrap agent %p against %s", agent, thisHostPort)
		err = agent.bootstrap(client, authMechanisms, nextAuth, srvDeadlineTm)
		if IsErrorStatus(err, StatusAuthError) ||
			IsErrorStatus(err, StatusAccessError) {
			agent.disconnectClient(client)
			return err
		} else if err != nil {
			logDebugf("Bootstrap failed %p/%s! %v", agent, thisHostPort, err)
			agent.disconnectClient(client)
			continue
		}
		logDebugf("Bootstrapped %p/%s", agent, thisHostPort)

		if agent.useCollections && !client.SupportsFeature(FeatureCollections) {
			logDebugf("Disabling collections as unsupported")
			agent.useCollections = false
		}

		if client.SupportsFeature(FeatureEnhancedDurability) {
			agent.durabilityLevelStatus = durabilityLevelStatusSupported
		} else {
			agent.durabilityLevelStatus = durabilityLevelStatusUnsupported
		}

		logDebugf("Attempting to request CCCP configuration")
		cfgBytes, err := syncCli.ExecGetClusterConfig(srvDeadlineTm)
		if err != nil {
			logDebugf("Failed to retrieve CCCP config %p/%s. %v", agent, thisHostPort, err)
			agent.disconnectClient(client)
			cccpUnsupported = true
			continue
		}

		hostName, err := hostFromHostPort(thisHostPort)
		if err != nil {
			logErrorf("Failed to parse CCCP source address %p/%s. %v", agent, thisHostPort, err)
			agent.disconnectClient(client)
			continue
		}

		bk, err := parseBktConfig(cfgBytes, hostName)
		if err != nil {
			logDebugf("Failed to parse cluster configuration %p/%s. %v", agent, thisHostPort, err)
			agent.disconnectClient(client)
			continue
		}

		if !bk.supportsCccp() {
			logDebugf("Bucket does not support CCCP %p/%s", agent, thisHostPort)
			agent.disconnectClient(client)
			cccpUnsupported = true
			break
		}

		routeCfg := agent.buildFirstRouteConfig(bk, thisHostPort)
		logDebugf("Using network type %s for connections", agent.networkType)
		if !routeCfg.IsValid() {
			logDebugf("Configuration was deemed invalid %+v", routeCfg)
			agent.disconnectClient(client)
			continue
		}

		agent.updateClusterCapabilities(bk)
		logDebugf("Successfully connected agent %p to %s", agent, thisHostPort)

		// Build some fake routing data, this is used to indicate that
		//  client is "alive".  A nil routeData causes immediate shutdown.
		agent.routingInfo.Update(nil, &routeData{
			revId: -1,
		})

		agent.cacheClientNoLock(client)

		if routeCfg.vbMap != nil {
			agent.numVbuckets = routeCfg.vbMap.NumVbuckets()
		} else {
			agent.numVbuckets = 0
		}

		agent.applyRoutingConfig(routeCfg)

		agent.cccpLooperDoneSig = make(chan struct{})
		go agent.cccpLooper()

		return nil
	}

	if cccpUnsupported {
		// We should only hit here if we're connecting to a memcached bucket.
		return agent.tryStartHttpLooper(httpAddrs)
	}

	// We failed to connect so start a mux with the provided addresses and keep trying to connect.
	mux := agent.newMemdClientMux(memdAddrs)
	agent.routingInfo.Update(nil, &routeData{
		revId:     -1,
		clientMux: mux,
	})
	mux.Start()

	return nil
}

func (agent *Agent) connectG3CP(memdAddrs, httpAddrs []string, authMechanisms []AuthMechanism, deadline time.Time) error {
	logDebugf("Attempting to connect %p...", agent)

	var routeCfg *routeConfig

	for _, thisHostPort := range memdAddrs {
		logDebugf("Trying server at %s for %p", thisHostPort, agent)

		srvDeadlineTm := time.Now().Add(agent.serverConnectTimeout)
		if srvDeadlineTm.After(deadline) {
			srvDeadlineTm = deadline
		}

		logDebugf("Trying to connect %p/%s", agent, thisHostPort)
		client, err := agent.dialMemdClient(thisHostPort, srvDeadlineTm)
		if err != nil {
			logDebugf("Connecting failed %p/%s! %v", agent, thisHostPort, err)
			continue
		}

		syncCli := syncClient{
			client: client,
		}

		var nextAuth func(mechanism AuthMechanism)
		if agent.authHandler == nil {
			nextAuth, err = agent.buildAuthHandler(&syncCli, authMechanisms, srvDeadlineTm)
			if err != nil {
				logDebugf("Building auth failed %p/%s! %v", agent, thisHostPort, err)
				continue
			}
		}

		logDebugf("Trying to bootstrap agent %p against %s", agent, thisHostPort)
		err = agent.bootstrap(client, authMechanisms, nextAuth, srvDeadlineTm)
		if IsErrorStatus(err, StatusAuthError) ||
			IsErrorStatus(err, StatusAccessError) {
			agent.disconnectClient(client)
			for _, cli := range agent.cachedClients {
				agent.disconnectClient(cli)
			}
			return err
		} else if err != nil {
			logDebugf("Bootstrap failed %p/%s! %v", agent, thisHostPort, err)
			agent.cacheClientNoLock(client)
			continue
		}
		logDebugf("Bootstrapped %p/%s", agent, thisHostPort)

		if agent.useCollections && !client.SupportsFeature(FeatureCollections) {
			logDebugf("Disabling collections as unsupported")
			agent.useCollections = false
		}

		if client.SupportsFeature(FeatureEnhancedDurability) {
			agent.durabilityLevelStatus = durabilityLevelStatusSupported
		} else {
			agent.durabilityLevelStatus = durabilityLevelStatusUnsupported
		}

		logDebugf("Attempting to request CCCP configuration")
		cfgBytes, err := syncCli.ExecGetClusterConfig(srvDeadlineTm)
		if err != nil {
			logDebugf("Failed to retrieve CCCP config %p/%s. %v", agent, thisHostPort, err)
			agent.cacheClientNoLock(client)
			continue
		}

		hostName, err := hostFromHostPort(thisHostPort)
		if err != nil {
			logErrorf("Failed to parse CCCP source address %p/%s. %v", agent, thisHostPort, err)
			agent.cacheClientNoLock(client)
			continue
		}

		cfg, err := parseClusterConfig(cfgBytes, hostName)
		if err != nil {
			logDebugf("Failed to parse cluster configuration %p/%s. %v", agent, thisHostPort, err)
			agent.cacheClientNoLock(client)
			continue
		}

		routeCfg = agent.buildFirstRouteConfig(cfg, thisHostPort)
		logDebugf("Using network type %s for connections", agent.networkType)
		if !routeCfg.IsValid() {
			logDebugf("Configuration was deemed invalid %+v", routeCfg)
			agent.disconnectClient(client)
			continue
		}

		agent.updateClusterCapabilities(cfg)
		logDebugf("Successfully connected agent %p to %s", agent, thisHostPort)
		agent.cacheClientNoLock(client)
	}

	if len(agent.cachedClients) == 0 {
		// If we're using gcccp or if we haven't failed due to cccp then fail.
		// TODO: If we want to support HTTP scheme for connect then we could do it here.
		logDebugf("No bucket selected and no clients cached, connect failed for %p", agent)
		return ErrBadHosts
	}

	// In the case of G3CP we don't need to worry about connecting over HTTP as there's no bucket.
	// If we've got cached clients then we made a connection and we want to use gcccp so no errors here.
	agent.cachedHTTPEndpoints = httpAddrs
	if routeCfg == nil {
		// No error but we don't support GCCCP.
		logDebugf("GCCCP unsupported, connections being held in trust.")
		return nil
	}
	agent.supportsGCCCP = true
	// Build some fake routing data, this is used to indicate that
	//  client is "alive".  A nil routeData causes immediate shutdown.
	agent.routingInfo.Update(nil, &routeData{
		revId: -1,
	})

	if routeCfg.vbMap != nil {
		agent.numVbuckets = routeCfg.vbMap.NumVbuckets()
	} else {
		agent.numVbuckets = 0
	}

	agent.applyRoutingConfig(routeCfg)

	agent.gcccpLooperDoneSig = make(chan struct{})
	agent.gcccpLooperStopSig = make(chan struct{})
	go agent.gcccpLooper()

	return nil

}

func (agent *Agent) disconnectClient(client *memdClient) {
	err := client.Close()
	if err != nil {
		logErrorf("Failed to shut down client connection (%s)", err)
	}
}

func (agent *Agent) cacheClientNoLock(client *memdClient) {
	agent.cachedClients[client.Address()] = client
}

func (agent *Agent) tryStartHttpLooper(httpAddrs []string) error {
	signal := make(chan error, 1)
	var routeCfg *routeConfig

	var epList []string
	for _, hostPort := range httpAddrs {
		if !agent.IsSecure() {
			epList = append(epList, fmt.Sprintf("http://%s", hostPort))
		} else {
			epList = append(epList, fmt.Sprintf("https://%s", hostPort))
		}
	}

	routingInfo := agent.routingInfo.Get()
	if routingInfo == nil {
		agent.routingInfo.Update(nil, &routeData{
			revId:      -1,
			mgmtEpList: epList,
		})
	}

	logDebugf("Starting HTTP looper! %v", epList)
	agent.httpLooperDoneSig = make(chan struct{})
	go agent.httpLooper(func(cfg *cfgBucket, srcServer string, err error) bool {
		if err != nil {
			signal <- err
			return true
		}

		if agent.useCollections && !cfg.supports("collections") {
			logDebugf("Disabling collections as unsupported")
			agent.useCollections = false
		}

		if cfg.supports("syncreplication") {
			agent.durabilityLevelStatus = durabilityLevelStatusSupported
		} else {
			agent.durabilityLevelStatus = durabilityLevelStatusUnsupported
		}

		newRouteCfg := agent.buildFirstRouteConfig(cfg, srcServer)
		if !newRouteCfg.IsValid() {
			// Something is invalid about this config, keep trying
			return false
		}

		agent.updateClusterCapabilities(cfg)
		routeCfg = newRouteCfg
		signal <- nil
		return true
	})

	err := <-signal
	if err != nil {
		return err
	}

	if routeCfg.vbMap != nil {
		agent.numVbuckets = routeCfg.vbMap.NumVbuckets()
	} else {
		agent.numVbuckets = 0
	}

	agent.applyRoutingConfig(routeCfg)

	return nil
}

func (agent *Agent) buildFirstRouteConfig(config cfgObj, srcServer string) *routeConfig {
	if agent.networkType != "" && agent.networkType != "auto" {
		return buildRouteConfig(config, agent.IsSecure(), agent.networkType, true)
	}

	defaultRouteConfig := buildRouteConfig(config, agent.IsSecure(), "default", true)

	// First we check if the source server is from the defaults list
	srcInDefaultConfig := false
	for _, endpoint := range defaultRouteConfig.kvServerList {
		if endpoint == srcServer {
			srcInDefaultConfig = true
		}
	}
	for _, endpoint := range defaultRouteConfig.mgmtEpList {
		if endpoint == srcServer {
			srcInDefaultConfig = true
		}
	}
	if srcInDefaultConfig {
		agent.networkType = "default"
		return defaultRouteConfig
	}

	// Next lets see if we have an external config, if so, default to that
	externalRouteCfg := buildRouteConfig(config, agent.IsSecure(), "external", true)
	if externalRouteCfg.IsValid() {
		agent.networkType = "external"
		return externalRouteCfg
	}

	// If all else fails, default to the implicit default config
	agent.networkType = "default"
	return defaultRouteConfig
}

func (agent *Agent) updateConfig(cfg cfgObj) {
	updated := agent.updateRoutingConfig(cfg)
	if !updated {
		return
	}

	agent.updateClusterCapabilities(cfg)
}

func (agent *Agent) getCachedClient(address string) *memdClient {
	agent.cachedClientsLock.Lock()
	cli, ok := agent.cachedClients[address]
	if !ok {
		agent.cachedClientsLock.Unlock()
		return nil
	}
	delete(agent.cachedClients, address)
	agent.cachedClientsLock.Unlock()

	return cli
}

// Close shuts down the agent, disconnecting from all servers and failing
// any outstanding operations with ErrShutdown.
func (agent *Agent) Close() error {
	agent.configLock.Lock()

	// Clear the routingInfo so no new operations are performed
	//   and retrieve the last active routing configuration
	routingInfo := agent.routingInfo.Clear()
	if routingInfo == nil {
		agent.configLock.Unlock()
		return ErrShutdown
	}

	// Notify everyone that we are shutting down
	close(agent.closeNotify)

	// Shut down the client multiplexer which will close all its queues
	// effectively causing all the clients to shut down.
	muxCloseErr := routingInfo.clientMux.Close()

	// Drain all the pipelines and error their requests, then
	//  drain the dead queue and error those requests.
	routingInfo.clientMux.Drain(func(req *memdQRequest) {
		req.tryCallback(nil, ErrShutdown)
	})

	agent.configLock.Unlock()

	agent.cachedClientsLock.Lock()
	for _, cli := range agent.cachedClients {
		err := cli.Close()
		if err != nil {
			logDebugf("Failed to close client %p", cli)
		}
	}
	agent.cachedClients = make(map[string]*memdClient)
	agent.cachedClientsLock.Unlock()

	// Wait for our external looper goroutines to finish, note that if the
	// specific looper wasn't used, it will be a nil value otherwise it
	// will be an open channel till its closed to signal completion.
	if agent.cccpLooperDoneSig != nil {
		<-agent.cccpLooperDoneSig
	}
	if agent.gcccpLooperDoneSig != nil {
		<-agent.gcccpLooperDoneSig
	}
	if agent.httpLooperDoneSig != nil {
		<-agent.httpLooperDoneSig
	}

	// Close the transports so that they don't hold open goroutines.
	if tsport, ok := agent.httpCli.Transport.(*http.Transport); ok {
		tsport.CloseIdleConnections()
	} else {
		logDebugf("Could not close idle connections for transport")
	}

	return muxCloseErr
}

// IsSecure returns whether this client is connected via SSL.
func (agent *Agent) IsSecure() bool {
	return agent.tlsConfig != nil
}

// BucketUUID returns the UUID of the bucket we are connected to.
func (agent *Agent) BucketUUID() string {
	routingInfo := agent.routingInfo.Get()
	if routingInfo == nil {
		return ""
	}

	return routingInfo.uuid
}

// KeyToVbucket translates a particular key to its assigned vbucket.
func (agent *Agent) KeyToVbucket(key []byte) uint16 {
	// TODO(brett19): The KeyToVbucket Bucket API should return an error

	routingInfo := agent.routingInfo.Get()
	if routingInfo == nil {
		return 0
	}

	if routingInfo.vbMap == nil {
		return 0
	}

	return routingInfo.vbMap.VbucketByKey(key)
}

// KeyToServer translates a particular key to its assigned server index.
func (agent *Agent) KeyToServer(key []byte, replicaIdx uint32) int {
	routingInfo := agent.routingInfo.Get()
	if routingInfo == nil {
		return -1
	}

	if routingInfo.vbMap != nil {
		serverIdx, err := routingInfo.vbMap.NodeByKey(key, replicaIdx)
		if err != nil {
			return -1
		}

		return serverIdx
	}

	if routingInfo.ketamaMap != nil {
		serverIdx, err := routingInfo.ketamaMap.NodeByKey(key)
		if err != nil {
			return -1
		}

		return serverIdx
	}

	return -1
}

// VbucketToServer returns the server index for a particular vbucket.
func (agent *Agent) VbucketToServer(vbID uint16, replicaIdx uint32) int {
	routingInfo := agent.routingInfo.Get()
	if routingInfo == nil {
		return -1
	}

	if routingInfo.vbMap == nil {
		return -1
	}

	serverIdx, err := routingInfo.vbMap.NodeByVbucket(vbID, replicaIdx)
	if err != nil {
		return -1
	}

	return serverIdx
}

// NumVbuckets returns the number of VBuckets configured on the
// connected cluster.
func (agent *Agent) NumVbuckets() int {
	return agent.numVbuckets
}

func (agent *Agent) bucketType() bucketType {
	routingInfo := agent.routingInfo.Get()
	if routingInfo == nil {
		return bktTypeInvalid
	}

	return routingInfo.bktType
}

// NumReplicas returns the number of replicas configured on the
// connected cluster.
func (agent *Agent) NumReplicas() int {
	routingInfo := agent.routingInfo.Get()
	if routingInfo == nil {
		return 0
	}

	if routingInfo.vbMap == nil {
		return 0
	}

	return routingInfo.vbMap.NumReplicas()
}

// NumServers returns the number of servers accessible for K/V.
func (agent *Agent) NumServers() int {
	routingInfo := agent.routingInfo.Get()
	if routingInfo == nil {
		return 0
	}
	return routingInfo.clientMux.NumPipelines()
}

// TODO(brett19): Update VbucketsOnServer to return all servers.
// Otherwise, we could race the route map update and get a
// non-continuous list of vbuckets for each server.

// VbucketsOnServer returns the list of VBuckets for a server.
func (agent *Agent) VbucketsOnServer(index int) []uint16 {
	routingInfo := agent.routingInfo.Get()
	if routingInfo == nil {
		return nil
	}

	if routingInfo.vbMap == nil {
		return nil
	}

	vbList := routingInfo.vbMap.VbucketsByServer(0)

	if len(vbList) <= index {
		// Invalid server index
		return nil
	}

	return vbList[index]
}

// ClientId returns the unique id for this agent
func (agent *Agent) ClientId() string {
	return agent.clientId
}

// CapiEps returns all the available endpoints for performing
// map-reduce queries.
func (agent *Agent) CapiEps() []string {
	routingInfo := agent.routingInfo.Get()
	if routingInfo == nil {
		return nil
	}
	return routingInfo.capiEpList
}

// MgmtEps returns all the available endpoints for performing
// management queries.
func (agent *Agent) MgmtEps() []string {
	routingInfo := agent.routingInfo.Get()
	if routingInfo == nil {
		return nil
	}
	return routingInfo.mgmtEpList
}

// N1qlEps returns all the available endpoints for performing
// N1QL queries.
func (agent *Agent) N1qlEps() []string {
	routingInfo := agent.routingInfo.Get()
	if routingInfo == nil {
		return nil
	}
	return routingInfo.n1qlEpList
}

// FtsEps returns all the available endpoints for performing
// FTS queries.
func (agent *Agent) FtsEps() []string {
	routingInfo := agent.routingInfo.Get()
	if routingInfo == nil {
		return nil
	}
	return routingInfo.ftsEpList
}

// CbasEps returns all the available endpoints for performing
// CBAS queries.
func (agent *Agent) CbasEps() []string {
	routingInfo := agent.routingInfo.Get()
	if routingInfo == nil {
		return nil
	}
	return routingInfo.cbasEpList
}

// HasCollectionsSupport verifies whether or not collections are available on the agent.
func (agent *Agent) HasCollectionsSupport() bool {
	return agent.useCollections
}

// UsingGCCCP returns whether or not the Agent is currently using GCCCP polling.
func (agent *Agent) UsingGCCCP() bool {
	return agent.supportsGCCCP
}

func (agent *Agent) bucket() string {
	agent.bucketLock.Lock()
	defer agent.bucketLock.Unlock()
	return agent.bucketName
}

func (agent *Agent) setBucket(bucket string) {
	agent.bucketLock.Lock()
	defer agent.bucketLock.Unlock()
	agent.bucketName = bucket
}

// SelectBucket performs a select bucket operation against the cluster.
func (agent *Agent) SelectBucket(bucketName string, deadline time.Time) error {
	if agent.bucket() != "" {
		return ErrBucketAlreadySelected
	}

	logDebugf("Selecting on %p", agent)

	// Stop the gcccp looper if it's running, if we connected to a node but gcccp wasn't supported then the looper
	// won't be running.
	if agent.gcccpLooperStopSig != nil {
		agent.gcccpLooperStopSig <- struct{}{}
		<-agent.gcccpLooperDoneSig
		logDebugf("GCCCP poller halted for %p", agent)
	}

	agent.setBucket(bucketName)
	var routeCfg *routeConfig

	if agent.UsingGCCCP() {
		routingInfo := agent.routingInfo.Get()
		if routingInfo != nil {
			// We should only get here if cachedClients was empty.
			for i := 0; i < routingInfo.clientMux.NumPipelines(); i++ {
				// Each pipeline should only have 1 connection whilst using GCCCP.
				pipeline := routingInfo.clientMux.GetPipeline(i)
				client := syncClient{
					client: &memdPipelineSenderWrap{
						pipeline: pipeline,
					},
				}
				logDebugf("Selecting bucket against pipeline %p/%s", pipeline, pipeline.Address())

				_, err := client.doBasicOp(cmdSelectBucket, []byte(bucketName), nil, nil, deadline)
				if err != nil {
					// This means that we can't connect to the bucket because something is invalid so bail.
					if IsErrorStatus(err, StatusAccessError) {
						agent.setBucket("")
						return err
					}

					// Otherwise close the pipeline and let the later config refresh create a new set of connections to this
					// node.
					logDebugf("Shutting down pipeline %s/%p after failing to select bucket", pipeline.Address(), pipeline)
					err = pipeline.Close()
					if err != nil {
						logDebugf("Failed to shutdown pipeline %s/%p (%v)", pipeline.Address(), pipeline, err)
					}
					continue
				}
				logDebugf("Bucket selected successfully against pipeline %p/%s", pipeline, pipeline.Address())

				if routeCfg == nil {
					cccpBytes, err := client.ExecGetClusterConfig(deadline)
					if err != nil {
						logDebugf("CCCPPOLL: Failed to retrieve CCCP config. %v", err)
						continue
					}

					hostName, err := hostFromHostPort(pipeline.Address())
					if err != nil {
						logErrorf("CCCPPOLL: Failed to parse source address. %v", err)
						continue
					}

					bk, err := parseBktConfig(cccpBytes, hostName)
					if err != nil {
						logDebugf("CCCPPOLL: Failed to parse CCCP config. %v", err)
						continue
					}

					routeCfg = buildRouteConfig(bk, agent.IsSecure(), agent.networkType, false)
					if !routeCfg.IsValid() {
						logDebugf("Configuration was deemed invalid %+v", routeCfg)
						routeCfg = nil
						continue
					}
				}
			}
		}
	} else {
		// We don't need to keep the lock on this, if we have cached clients then we don't support gcccp so no pipelines are running.
		agent.cachedClientsLock.Lock()
		clients := agent.cachedClients
		agent.cachedClientsLock.Unlock()

		for _, cli := range clients {
			// waitCh := make(chan error)
			client := syncClient{
				client: cli,
			}

			logDebugf("Selecting bucket against client %p/%s", cli, cli.Address())

			_, err := client.doBasicOp(cmdSelectBucket, []byte(bucketName), nil, nil, deadline)
			if err != nil {
				// This means that we can't connect to the bucket because something is invalid so bail.
				if IsErrorStatus(err, StatusAccessError) {
					agent.setBucket("")
					return err
				}

				// Otherwise keep the client around and it'll get used for pipeline client later, it might connect correctly
				// later.
				continue
			}
			logDebugf("Bucket selected successfully against client %p/%s", cli, cli.Address())

			if routeCfg == nil {
				cccpBytes, err := client.ExecGetClusterConfig(deadline)
				if err != nil {
					logDebugf("CCCPPOLL: Failed to retrieve CCCP config. %v", err)
					continue
				}

				hostName, err := hostFromHostPort(cli.Address())
				if err != nil {
					logErrorf("CCCPPOLL: Failed to parse source address. %v", err)
					continue
				}

				bk, err := parseBktConfig(cccpBytes, hostName)
				if err != nil {
					logDebugf("CCCPPOLL: Failed to parse CCCP config. %v", err)
					continue
				}

				routeCfg = agent.buildFirstRouteConfig(bk, cli.Address())
				if !routeCfg.IsValid() {
					logDebugf("Configuration was deemed invalid %+v", routeCfg)
					routeCfg = nil
					continue
				}
			}
		}
	}

	if routeCfg == nil || !routeCfg.IsValid() {
		logDebugf("No valid route config created, starting HTTP looper.")
		// If we failed to get a routeCfg then try the http looper instead, this will be the case for memcached buckets.
		err := agent.tryStartHttpLooper(agent.cachedHTTPEndpoints)
		if err != nil {
			agent.setBucket("")
			return err
		}
		return nil
	}

	routingInfo := agent.routingInfo.Get()
	if routingInfo == nil {
		// Build some fake routing data, this is used to indicate that
		// client is "alive".  A nil routeData causes immediate shutdown.
		// If we don't support GCCP then we could hit this.
		agent.routingInfo.Update(nil, &routeData{
			revId: -1,
		})
	}

	// We need to update the numVbuckets as previously they would have been 0 even if we had been gcccp looping
	if routeCfg.vbMap != nil {
		agent.numVbuckets = routeCfg.vbMap.NumVbuckets()
	} else {
		agent.numVbuckets = 0
	}

	agent.applyRoutingConfig(routeCfg)

	logDebugf("Select bucket completed, starting CCCP looper.")

	agent.cccpLooperDoneSig = make(chan struct{})
	go agent.cccpLooper()
	return nil
}

func (agent *Agent) newMemdClientMux(hostPorts []string) *memdClientMux {
	if agent.bucket() == "" {
		return newMemdClientMux(hostPorts, 1, agent.maxQueueSize, agent.slowDialMemdClient)
	}

	return newMemdClientMux(hostPorts, agent.kvPoolSize, agent.maxQueueSize, agent.slowDialMemdClient)
}

// HasRetrievedConfig verifies that the agent has, at some point in its lifetime, been able to connect to the cluster
// at some point and has managed to retrieve a cluster config. It does not necessarily mean that it still
// connected right now.
func (agent *Agent) HasRetrievedConfig() bool {
	eps := agent.MgmtEps()
	if eps == nil || len(eps) == 0 {
		return false
	}

	return true
}
