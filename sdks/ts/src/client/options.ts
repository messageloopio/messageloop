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
 * With encoding option.
 */
export function withEncoding(encoding: "json" | "proto"): ClientOption {
  return (options: ClientOptions) => {
    options.encoding = encoding;
  };
}

/**
 * With client ID option.
 */
export function withClientId(clientId: string): ClientOption {
  return (options: ClientOptions) => {
    options.clientId = clientId;
  };
}

/**
 * With client type option.
 */
export function withClientType(clientType: string): ClientOption {
  return (options: ClientOptions) => {
    options.clientType = clientType;
  };
}

/**
 * With authentication token option.
 */
export function withToken(token: string): ClientOption {
  return (options: ClientOptions) => {
    options.token = token;
  };
}

/**
 * With client version option.
 */
export function withVersion(version: string): ClientOption {
  return (options: ClientOptions) => {
    options.version = version;
  };
}

/**
 * With auto-subscribe channels option.
 */
export function withAutoSubscribe(...channels: string[]): ClientOption {
  return (options: ClientOptions) => {
    options.autoSubscribe = channels;
  };
}

/**
 * With ping interval option.
 */
export function withPingInterval(interval: number): ClientOption {
  return (options: ClientOptions) => {
    options.pingInterval = interval;
  };
}

/**
 * With ping timeout option.
 */
export function withPingTimeout(timeout: number): ClientOption {
  return (options: ClientOptions) => {
    options.pingTimeout = timeout;
  };
}

/**
 * With connection timeout option.
 */
export function withConnectTimeout(timeout: number): ClientOption {
  return (options: ClientOptions) => {
    options.connectTimeout = timeout;
  };
}

/**
 * With RPC timeout option.
 */
export function withRPCTimeout(timeout: number): ClientOption {
  return (options: ClientOptions) => {
    options.rpcTimeout = timeout;
  };
}

/**
 * With ephemeral subscriptions option.
 */
export function withEphemeral(ephemeral: boolean): ClientOption {
  return (options: ClientOptions) => {
    options.ephemeral = ephemeral;
  };
}

/**
 * With auto-reconnect option.
 */
export function withAutoReconnect(enabled: boolean): ClientOption {
  return (options: ClientOptions) => {
    options.autoReconnect = enabled;
  };
}

/**
 * With reconnection delay options.
 * @param initial - Initial delay in milliseconds
 * @param max - Maximum delay in milliseconds
 */
export function withReconnectDelay(initial: number, max: number): ClientOption {
  return (options: ClientOptions) => {
    options.reconnectInitialDelay = initial;
    options.reconnectMaxDelay = max;
  };
}

/**
 * With maximum reconnection attempts.
 * @param attempts - Maximum attempts (0 = unlimited)
 */
export function withReconnectMaxAttempts(attempts: number): ClientOption {
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
