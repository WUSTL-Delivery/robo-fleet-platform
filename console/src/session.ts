// The console's one connection to fleet-server, built on the SDK's FleetClient.
// Credentials live in localStorage (LocalStorageTokenStore), so a reload
// reconnects with the same operator identity instead of asking again.
import { FleetClient, LocalStorageTokenStore, type WelcomePayload } from "@fleet-platform/sdk";

export const tokenStore = new LocalStorageTokenStore("fleet-console.credentials");

/** How the operator signs in: redeem a one-time invite, or reuse a token. */
export type SignIn =
  | { kind: "invite"; inviteKey: string; name: string }
  | { kind: "token"; token: string }
  | { kind: "stored" };

/** The server's WebSocket endpoint: same origin as the console. */
export function wsURL(loc: Location = window.location): string {
  const scheme = loc.protocol === "https:" ? "wss:" : "ws:";
  return `${scheme}//${loc.host}/ws`;
}

export function hasStoredCredentials(): boolean {
  return tokenStore.load() !== null;
}

/**
 * Creates (but does not connect) the client for a sign-in. A pasted token is
 * written to the store first so the client skips enrollment; its client_id and
 * fleet_id are filled in from the first welcome (see rememberIdentity).
 */
export function createClient(signIn: SignIn): FleetClient {
  if (signIn.kind === "token") {
    tokenStore.save({ token: signIn.token, client_id: "", fleet_id: "" });
  } else if (signIn.kind === "invite") {
    tokenStore.clear();
  }
  return new FleetClient({
    url: wsURL(),
    kind: "operator",
    ...(signIn.kind === "invite" ? { enrollmentKey: signIn.inviteKey, name: signIn.name || "operator" } : {}),
    tokenStore,
    agent: { name: "fleet-console", version: __CONSOLE_VERSION__ },
  });
}

/** Completes stored credentials from a welcome (needed after a pasted token). */
export function rememberIdentity(welcome: WelcomePayload): void {
  const creds = tokenStore.load();
  if (creds && (creds.client_id !== welcome.client_id || creds.fleet_id !== welcome.fleet_id)) {
    tokenStore.save({ ...creds, client_id: welcome.client_id, fleet_id: welcome.fleet_id });
  }
}

export function signOut(): void {
  tokenStore.clear();
}
