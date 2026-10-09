// Layer 1 contract tests (docs/TESTING.md): the TS side of the cross-language
// handshake. Validates every golden fixture in protocol/fixtures against the same
// JSON Schemas and catalog.json the Go server uses
// (sdk/go/protocol/contract_test.go): envelope schema, known type,
// then the payload schema the catalog maps that type to.
import { readdirSync, readFileSync } from "node:fs";
import { Ajv2020, type ValidateFunction } from "ajv/dist/2020.js";
import { describe, expect, it } from "vitest";
import { MESSAGE_TYPES } from "../src/index.js";

const protocolDir = new URL("../../../protocol/", import.meta.url);
const schemaBase = "https://fleetplatform.local/v0/";

type Catalog = { v: number; envelope: string; messages: Record<string, string> };

function readJSON(url: URL): unknown {
  return JSON.parse(readFileSync(url, "utf8"));
}

function listJSON(dir: URL): string[] {
  return readdirSync(dir)
    .filter((f) => f.endsWith(".json"))
    .sort();
}

const catalog = readJSON(new URL("catalog.json", protocolDir)) as Catalog;

type Validators = {
  /** Returns null when `msg` is a valid envelope with a valid payload, else a reason. */
  check(msg: unknown): string | null;
  byType: Map<string, ValidateFunction>;
};

function loadValidators(): Validators {
  // Formats are not asserted, matching the Go validator's default.
  const ajv = new Ajv2020({
    allErrors: true,
    // Strict catches unknown keywords (schema typos); the two relaxed checks flag
    // idioms the schemas use on purpose: oneOf branches that only list `required`,
    // and union `type` arrays.
    strict: true,
    strictRequired: false,
    allowUnionTypes: true,
  });
  const schemaDir = new URL("schemas/", protocolDir);
  const schemaFiles = listJSON(schemaDir).filter((f) => f.endsWith(".schema.json"));
  if (schemaFiles.length === 0) throw new Error("no schemas found");
  for (const f of schemaFiles) {
    ajv.addSchema(readJSON(new URL(f, schemaDir)) as object, schemaBase + f);
  }

  const compile = (ref: string): ValidateFunction => {
    const id = schemaBase + ref.replace(/^schemas\//, "");
    const fn = ajv.getSchema(id);
    if (!fn) throw new Error(`catalog ref does not resolve: ${ref}`);
    return fn;
  };

  const envelope = compile(catalog.envelope);
  const byType = new Map<string, ValidateFunction>();
  for (const [type, ref] of Object.entries(catalog.messages)) byType.set(type, compile(ref));

  const reason = (fn: ValidateFunction) => ajv.errorsText(fn.errors);
  return {
    byType,
    check(msg) {
      if (!envelope(msg)) return `envelope: ${reason(envelope)}`;
      const { type, payload } = msg as { type: string; payload: unknown };
      const fn = byType.get(type);
      if (!fn) return `unknown message type: ${type}`;
      if (!fn(payload)) return `${type} payload: ${reason(fn)}`;
      return null;
    },
  };
}

const validDir = new URL("fixtures/valid/", protocolDir);
const invalidDir = new URL("fixtures/invalid/", protocolDir);
const validFixtures = listJSON(validDir);
const invalidFixtures = listJSON(invalidDir);
const v = loadValidators();

describe("protocol contract: shared fixtures", () => {
  it("finds fixtures in both directories", () => {
    expect(validFixtures.length).toBeGreaterThan(0);
    expect(invalidFixtures.length).toBeGreaterThan(0);
  });

  describe("valid fixtures validate", () => {
    it.each(validFixtures)("%s", (f) => {
      expect(v.check(readJSON(new URL(f, validDir)))).toBeNull();
    });
  });

  describe("invalid fixtures are rejected", () => {
    it.each(invalidFixtures)("%s", (f) => {
      expect(v.check(readJSON(new URL(f, invalidDir))), "fixture in fixtures/invalid/ unexpectedly validated").not.toBeNull();
    });
  });
});

describe("protocol contract: catalog coverage", () => {
  it("SDK message types match the catalog exactly", () => {
    expect(new Set(MESSAGE_TYPES)).toEqual(new Set(v.byType.keys()));
  });

  it("every valid fixture's type is a catalog type", () => {
    for (const f of validFixtures) {
      const { type } = readJSON(new URL(f, validDir)) as { type: string };
      expect(v.byType.has(type), `${f}: type ${type}`).toBe(true);
    }
  });
});
