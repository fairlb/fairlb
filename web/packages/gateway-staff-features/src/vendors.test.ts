import { describe, expect, it } from "vitest";
import { prefillFromVendor, vendorBySlug, vendorLabel, type Vendor } from "./vendors";

// What a vendor choice fills in is a pure function, so it is asserted here
// rather than through the form: the form's job is to route the answer into
// fields, and testing it there would test both at once and pin neither.

const deepseek: Vendor = {
  slug: "deepseek",
  label: "DeepSeek",
  kind: "first_party",
  protocols: ["openai", "anthropic"],
  default_protocols: ["openai"],
  base_urls: [{ url: "https://api.deepseek.com", template: false }],
  transport: { path_overrides: { "/v1/messages": "/anthropic/v1/messages" } },
  key_hint: "bearer",
  model_listing: true,
};

const bedrock: Vendor = {
  slug: "aws-bedrock",
  label: "AWS Bedrock",
  kind: "platform",
  protocols: ["anthropic"],
  default_protocols: ["anthropic"],
  base_urls: [{ url: "https://bedrock-runtime.{region}.amazonaws.com", template: true }],
  transport: { auth: "aws_sigv4" },
  key_hint: "aws_keypair_json",
  model_listing: false,
};

const moonshot: Vendor = {
  slug: "moonshot",
  label: "Moonshot AI",
  kind: "first_party",
  protocols: ["openai"],
  default_protocols: ["openai"],
  base_urls: [
    { label: "China", url: "https://api.moonshot.cn", template: false },
    { label: "International", url: "https://api.moonshot.ai", template: false },
  ],
  key_hint: "bearer",
  model_listing: true,
};

describe("prefillFromVendor", () => {
  it("fills the endpoint, the default protocols and the preset", () => {
    const fill = prefillFromVendor(deepseek, []);
    expect(fill.slug).toBe("deepseek");
    expect(fill.baseUrl).toBe("https://api.deepseek.com");
    // The preset's defaults, not everything the platform publishes: a protocol
    // outside the default set is real but needs its own base URL or transport
    // profile, so ticking it is a deliberate choice. (This fixture's preset is
    // wired for one of its two protocols; the real registry says which.)
    expect(fill.protocols).toEqual(["openai"]);
    expect(JSON.parse(fill.transportText)).toEqual({
      path_overrides: { "/v1/messages": "/anthropic/v1/messages" },
    });
    expect(fill.baseUrlNeedsEditing).toBe(false);
  });

  it("suggests a free slug rather than one that would be refused on save", () => {
    expect(prefillFromVendor(deepseek, ["deepseek"]).slug).toBe("deepseek-2");
    expect(prefillFromVendor(deepseek, ["deepseek", "deepseek-2"]).slug).toBe("deepseek-3");
  });

  it("marks a placeholder endpoint as still needing an answer", () => {
    const fill = prefillFromVendor(bedrock, []);
    expect(fill.baseUrl).toContain("{region}");
    // The flag is what lets the form say so where the value is; without it the
    // placeholder reaches the server and comes back as a validation error about
    // a field nobody typed.
    expect(fill.baseUrlNeedsEditing).toBe(true);
  });

  it("takes the endpoint the region names, and the first when none is named", () => {
    expect(prefillFromVendor(moonshot, [], "International").baseUrl).toBe(
      "https://api.moonshot.ai",
    );
    expect(prefillFromVendor(moonshot, []).baseUrl).toBe("https://api.moonshot.cn");
  });

  it("fills in nothing at all for no vendor", () => {
    const fill = prefillFromVendor(undefined, []);
    expect(fill).toEqual({
      slug: "",
      baseUrl: "",
      protocols: [],
      transportText: "",
      baseUrlNeedsEditing: false,
    });
  });

  it("leaves the transport empty for a vendor that needs none", () => {
    expect(prefillFromVendor(moonshot, []).transportText).toBe("");
  });
});

describe("vendorLabel", () => {
  it("shows the platform's name", () => {
    expect(vendorLabel("deepseek", [deepseek])).toBe("DeepSeek");
  });

  it("falls back to the slug while the registry is unknown", () => {
    // Undecided query and unknown vendor both land here. Showing the slug is
    // ugly; showing nothing puts a blank where an identity belongs.
    expect(vendorLabel("deepseek", undefined)).toBe("deepseek");
    expect(vendorLabel("never-heard-of-it", [deepseek])).toBe("never-heard-of-it");
  });

  it("finds an entry by slug, and reports absence rather than guessing", () => {
    expect(vendorBySlug("deepseek", [deepseek, moonshot])?.label).toBe("DeepSeek");
    expect(vendorBySlug("nope", [deepseek])).toBeUndefined();
  });
});
