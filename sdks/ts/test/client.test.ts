import {
  setEncoding,
  setClientId,
  setClientType,
  setToken,
  setVersion,
  setAutoSubscribe,
  setPingInterval,
  setPingTimeout,
  setConnectTimeout,
  setRPCTimeout,
  setEphemeral,
  buildClientOptions,
} from "../src/client/options";

describe("Client Options", () => {
  describe("setEncoding", () => {
    it("should set encoding to proto", () => {
      const options = buildClientOptions([setEncoding("proto")]);
      expect(options.encoding).toEqual("proto");
    });

    it("should set encoding to json", () => {
      const options = buildClientOptions([setEncoding("json")]);
      expect(options.encoding).toEqual("json");
    });
  });

  describe("setClientId", () => {
    it("should set custom client ID", () => {
      const options = buildClientOptions([setClientId("custom-id")]);
      expect(options.clientId).toEqual("custom-id");
    });
  });

  describe("setClientType", () => {
    it("should set client type", () => {
      const options = buildClientOptions([setClientType("web")]);
      expect(options.clientType).toEqual("web");
    });
  });

  describe("setToken", () => {
    it("should set authentication token", () => {
      const options = buildClientOptions([setToken("secret-token")]);
      expect(options.token).toEqual("secret-token");
    });
  });

  describe("setVersion", () => {
    it("should set client version", () => {
      const options = buildClientOptions([setVersion("2.0.0")]);
      expect(options.version).toEqual("2.0.0");
    });
  });

  describe("setAutoSubscribe", () => {
    it("should set auto-subscribe channels", () => {
      const options = buildClientOptions([setAutoSubscribe("ch1", "ch2")]);
      expect(options.autoSubscribe).toEqual(["ch1", "ch2"]);
    });
  });

  describe("setPingInterval", () => {
    it("should set ping interval", () => {
      const options = buildClientOptions([setPingInterval(15000)]);
      expect(options.pingInterval).toEqual(15000);
    });
  });

  describe("setPingTimeout", () => {
    it("should set ping timeout", () => {
      const options = buildClientOptions([setPingTimeout(5000)]);
      expect(options.pingTimeout).toEqual(5000);
    });
  });

  describe("setConnectTimeout", () => {
    it("should set connection timeout", () => {
      const options = buildClientOptions([setConnectTimeout(15000)]);
      expect(options.connectTimeout).toEqual(15000);
    });
  });

  describe("setRPCTimeout", () => {
    it("should set RPC timeout", () => {
      const options = buildClientOptions([setRPCTimeout(60000)]);
      expect(options.rpcTimeout).toEqual(60000);
    });
  });

  describe("setEphemeral", () => {
    it("should set ephemeral flag", () => {
      const options = buildClientOptions([setEphemeral(true)]);
      expect(options.ephemeral).toEqual(true);
    });
  });

  describe("buildClientOptions", () => {
    it("should apply all options", () => {
      const options = buildClientOptions([
        setEncoding("proto"),
        setClientId("my-client"),
        setClientType("web"),
        setToken("token123"),
        setVersion("1.1.0"),
        setAutoSubscribe("channel1"),
        setPingInterval(20000),
        setPingTimeout(8000),
        setConnectTimeout(20000),
        setRPCTimeout(45000),
        setEphemeral(true),
      ]);

      expect(options.encoding).toEqual("proto");
      expect(options.clientId).toEqual("my-client");
      expect(options.clientType).toEqual("web");
      expect(options.token).toEqual("token123");
      expect(options.version).toEqual("1.1.0");
      expect(options.autoSubscribe).toEqual(["channel1"]);
      expect(options.pingInterval).toEqual(20000);
      expect(options.pingTimeout).toEqual(8000);
      expect(options.connectTimeout).toEqual(20000);
      expect(options.rpcTimeout).toEqual(45000);
      expect(options.ephemeral).toEqual(true);
    });

    it("should generate random client ID if not set", () => {
      const options = buildClientOptions([]);
      expect(options.clientId).toBeDefined();
      expect(options.clientId.length).toBeGreaterThan(0);
    });

    it("should use default values for unset options", () => {
      const options = buildClientOptions([]);

      expect(options.encoding).toEqual("json");
      expect(options.clientType).toEqual("sdk");
      expect(options.version).toEqual("1.0.0");
      expect(options.autoSubscribe).toEqual([]);
      expect(options.pingInterval).toEqual(30000);
      expect(options.pingTimeout).toEqual(10000);
      expect(options.connectTimeout).toEqual(30000);
      expect(options.rpcTimeout).toEqual(30000);
      expect(options.ephemeral).toEqual(false);
    });
  });
});
