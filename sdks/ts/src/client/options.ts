/**
 * Client connection options.
 */
export interface ClientOptions {
  /** Message encoding: 'json' or 'proto' */
  encoding: "json" | "proto";
  /** Client identifier */
  clientId: string;
  /** Client type (e.g., 'mobile', 'web', 'sdk') */
  clientType: string;
  /** Authentication token */
  token: string;
  /** Client version */
  version: string;
  /** Channels to auto-subscribe on connect */
  autoSubscribe: string[];
  /** Heartbeat interval in milliseconds (0 to disable) */
  pingInterval: number;
  /** Pong timeout in milliseconds */
  pingTimeout: number;
  /** Connection timeout in milliseconds */
  connectTimeout: number;
  /** RPC timeout in milliseconds */
  rpcTimeout: number;
  /** Whether subscriptions are ephemeral */
  ephemeral: boolean;
  /** Enable automatic reconnection on disconnect */
  autoReconnect: boolean;
  /** Initial reconnection delay in milliseconds */
  reconnectInitialDelay: number;
  /** Maximum reconnection delay in milliseconds */
  reconnectMaxDelay: number;
  /** Maximum reconnection attempts (0 = unlimited) */
  reconnectMaxAttempts: number;
  /** Exponential backoff multiplier for reconnection delays */
  reconnectBackoffMultiplier: number;
}

/**
 * Default client options.
 */
const defaultOptions: Partial<ClientOptions> = {
  encoding: "json",
  clientId: "",
  clientType: "sdk",
  version: "1.0.0",
  autoSubscribe: [],
  pingInterval: 30000,
  pingTimeout: 10000,
  connectTimeout: 30000,
  rpcTimeout: 30000,
  ephemeral: false,
  autoReconnect: true,
  reconnectInitialDelay: 1000,
  reconnectMaxDelay: 30000,
  reconnectMaxAttempts: 0,
  reconnectBackoffMultiplier: 2,
};

/**
 * Client option setter function type.
 */
export type ClientOption = (options: ClientOptions) => void;

/**
 * Set encoding option.
 */
export function setEncoding(encoding: "json" | "proto"): ClientOption {
  return (options: ClientOptions) => {
    options.encoding = encoding;
  };
}

/**
 * Set client ID option.
 */
export function setClientId(clientId: string): ClientOption {
  return (options: ClientOptions) => {
    options.clientId = clientId;
  };
}

/**
 * Set client type option.
 */
export function setClientType(clientType: string): ClientOption {
  return (options: ClientOptions) => {
    options.clientType = clientType;
  };
}

/**
 * Set authentication token option.
 */
export function setToken(token: string): ClientOption {
  return (options: ClientOptions) => {
    options.token = token;
  };
}

/**
 * Set client version option.
 */
export function setVersion(version: string): ClientOption {
  return (options: ClientOptions) => {
    options.version = version;
  };
}

/**
 * Set auto-subscribe channels option.
 */
export function setAutoSubscribe(...channels: string[]): ClientOption {
  return (options: ClientOptions) => {
    options.autoSubscribe = channels;
  };
}

/**
 * Set ping interval option.
 */
export function setPingInterval(interval: number): ClientOption {
  return (options: ClientOptions) => {
    options.pingInterval = interval;
  };
}

/**
 * Set ping timeout option.
 */
export function setPingTimeout(timeout: number): ClientOption {
  return (options: ClientOptions) => {
    options.pingTimeout = timeout;
  };
}

/**
 * Set connection timeout option.
 */
export function setConnectTimeout(timeout: number): ClientOption {
  return (options: ClientOptions) => {
    options.connectTimeout = timeout;
  };
}

/**
 * Set RPC timeout option.
 */
export function setRPCTimeout(timeout: number): ClientOption {
  return (options: ClientOptions) => {
    options.rpcTimeout = timeout;
  };
}

/**
 * Set ephemeral subscriptions option.
 */
export function setEphemeral(ephemeral: boolean): ClientOption {
  return (options: ClientOptions) => {
    options.ephemeral = ephemeral;
  };
}

/**
 * Set auto-reconnect option.
 */
export function setAutoReconnect(enabled: boolean): ClientOption {
  return (options: ClientOptions) => {
    options.autoReconnect = enabled;
  };
}

/**
 * Set reconnection delay options.
 * @param initial - Initial delay in milliseconds
 * @param max - Maximum delay in milliseconds
 */
export function setReconnectDelay(initial: number, max: number): ClientOption {
  return (options: ClientOptions) => {
    options.reconnectInitialDelay = initial;
    options.reconnectMaxDelay = max;
  };
}

/**
 * Set maximum reconnection attempts.
 * @param attempts - Maximum attempts (0 = unlimited)
 */
export function setReconnectMaxAttempts(attempts: number): ClientOption {
  return (options: ClientOptions) => {
    options.reconnectMaxAttempts = attempts;
  };
}

/**
 * Build client options from option setters.
 */
export function buildClientOptions(
  setters: ClientOption[] = []
): ClientOptions {
  const options: ClientOptions = {
    encoding: defaultOptions.encoding!,
    clientId: defaultOptions.clientId!,
    clientType: defaultOptions.clientType!,
    token: "",
    version: defaultOptions.version!,
    autoSubscribe: defaultOptions.autoSubscribe!,
    pingInterval: defaultOptions.pingInterval!,
    pingTimeout: defaultOptions.pingTimeout!,
    connectTimeout: defaultOptions.connectTimeout!,
    rpcTimeout: defaultOptions.rpcTimeout!,
    ephemeral: defaultOptions.ephemeral!,
    autoReconnect: defaultOptions.autoReconnect!,
    reconnectInitialDelay: defaultOptions.reconnectInitialDelay!,
    reconnectMaxDelay: defaultOptions.reconnectMaxDelay!,
    reconnectMaxAttempts: defaultOptions.reconnectMaxAttempts!,
    reconnectBackoffMultiplier: defaultOptions.reconnectBackoffMultiplier!,
  };

  // Generate random client ID if not provided
  if (!options.clientId) {
    options.clientId = crypto.randomUUID();
  }

  // Apply all option setters
  for (const setter of setters) {
    setter(options);
  }

  return options;
}
