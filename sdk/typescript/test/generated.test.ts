import { readFileSync } from "node:fs";
import { describe, expect, expectTypeOf, it } from "vitest";
import { MESSAGE_TYPES, PROTOCOL_VERSION, type AnyEnvelope, type TwistPayload } from "../src/index.js";

const catalog = JSON.parse(
  readFileSync(new URL("../../../protocol/catalog.json", import.meta.url), "utf8"),
) as { v: number; messages: Record<string, string> };

describe("generated protocol types", () => {
  it("cover exactly the catalog's message types", () => {
    expect([...MESSAGE_TYPES]).toEqual(Object.keys(catalog.messages));
    expect(PROTOCOL_VERSION).toBe(catalog.v);
  });

  it("narrow an envelope's payload by its type", () => {
    const env: AnyEnvelope = {
      v: 0,
      type: "twist",
      payload: { lease_id: "lease-1", linear: { x_mps: 0.5 }, angular: { z_radps: 0 } },
    };
    if (env.type === "twist") expectTypeOf(env.payload).toEqualTypeOf<TwistPayload>();
    expect(env.type).toBe("twist");
  });
});
