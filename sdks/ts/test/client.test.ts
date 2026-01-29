import {
  withEncoding,
  withClientId,
  withClientType,
  withToken,
  withVersion,
  withAutoSubscribe,
  withPingInterval,
  withPingTimeout,
  withConnectTimeout,
  withRPCTimeout,
  withEphemeral,
  buildClientOptions,
} from "../src/client/options";

describe("Client Options", () => {
  describe("withEncoding", () => {
    it("should set encoding to proto", () => {
      const options = buildClientOptions([withEncoding("proto")]);
      expect(options.encoding).toEqual("proto");
    });

    it("should set encoding to json", () => {
      const options = buildClientOptions([withEncoding("json")]);
      expect(options.encoding).toEqual("json");
    });
  });

  describe("withClientId", () => {
    it("should set custom client ID", () => {
      const options = buildClientOptions([withClientId("custom-id")]);
      expect(options.clientId).toEqual("custom-id");
    });
  });

  describe("withClientType", () => {
    it("should set client type", () => {
      const options = buildClientOptions([withClientType("web")]);
      expect(options.clientType).toEqual("web");
    });
  });

  describe("withToken", () => {
    it("should set authentication token", () => {
      const options = buildClientOptions([withToken("secret-token")]);
      expect(options.token).toEqual("secret-token");
    });
  });

  describe("withVersion", () => {
    it("should set client version", () => {
      const options = buildClientOptions([withVersion("2.0.0")]);
      expect(options.version).toEqual("2.0.0");
    });
  });

  describe("withAutoSubscribe", () => {
    it("should set auto-subscribe channels", () => {
      const options = buildClientOptions([withAutoSubscribe("ch1", "ch2")]);
      expect(options.autoSubscribe).toEqual(["ch1", "ch2"]);
    });
  });

  describe("withPingInterval", () => {
    it("should set ping interval", () => {
      const options = buildClientOptions([withPingInterval(15000)]);
      expect(options.pingInterval).toEqual(15000);
    });
  });

  describe("withPingTimeout", () => {
    it("should set ping timeout", () => {
      const options = buildClientOptions([withPingTimeout(5000)]);
      expect(options.pingTimeout).toEqual(5000);
    });
  });

  describe("withConnectTimeout", () => {
    it("should set connection timeout", () => {
      const options = buildClientOptions([withConnectTimeout(15000)]);
      expect(options.connectTimeout).toEqual(15000);
    });
  });

  describe("withRPCTimeout", () => {
    it("should set RPC timeout", () => {
      const options = buildClientOptions([withRPCTimeout(60000)]);
      expect(options.rpcTimeout).toEqual(60000);
    });
  });

  describe("withEphemeral", () => {
    it("should set ephemeral flag", () => {
      const options = buildClientOptions([withEphemeral(true)]);
      expect(options.ephemeral).toEqual(true);
    });
  });

  describe("buildClientOptions", () => {
    it("should apply all options", () => {
      const options = buildClientOptions([
        withEncoding("proto"),
        withClientId("my-client"),
        withClientType("web"),
        withToken("token123"),
        withVersion("1.1.0"),
        withAutoSubscribe("channel1"),
        withPingInterval(20000),
        withPingTimeout(8000),
        withConnectTimeout(20000),
        withRPCTimeout(45000),
        withEphemeral(true),
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
