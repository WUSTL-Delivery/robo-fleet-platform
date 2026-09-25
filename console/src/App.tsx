import { useCallback, useEffect, useRef, useState } from "react";
import type { ConnectionState, FleetClient, WelcomePayload } from "@fleet-platform/sdk";
import { createClient, hasStoredCredentials, rememberIdentity, signOut, type SignIn } from "./session";
import { Login } from "./Login";
import { Connected } from "./Connected";

export interface SessionView {
  state: ConnectionState;
  welcome?: WelcomePayload;
  error?: string;
  retryInMs?: number;
}

export function App() {
  const clientRef = useRef<FleetClient | null>(null);
  const [session, setSession] = useState<SessionView | null>(null);
  const [loginError, setLoginError] = useState<string>();

  const start = useCallback((signIn: SignIn) => {
    clientRef.current?.close();
    setLoginError(undefined);
    const client = createClient(signIn);
    clientRef.current = client;
    setSession({ state: "idle" });
    let welcomed = false;
    client.onState((change) => {
      if (clientRef.current !== client) return;
      if (change.state === "open" && change.welcome) {
        welcomed = true;
        rememberIdentity(change.welcome);
      }
      // A refused identity (bad invite, revoked token, taken over) goes back to
      // the login screen with the reason; anything else keeps the session view.
      if (change.state === "closed" && change.error && change.error.code !== "closed") {
        if (change.error.code === "auth_failed") signOut();
        clientRef.current = null;
        setSession(null);
        setLoginError(describe(change.error.code, change.error.message, welcomed));
        return;
      }
      setSession((prev) => ({
        state: change.state,
        welcome: change.welcome ?? prev?.welcome,
        error: change.error?.message,
        retryInMs: change.retryInMs,
      }));
    });
    void client.connect().catch(() => {
      /* surfaced through onState */
    });
  }, []);

  // Reconnect on load when this browser already holds an operator token.
  useEffect(() => {
    if (hasStoredCredentials()) start({ kind: "stored" });
    return () => clientRef.current?.close();
  }, [start]);

  const logout = useCallback(() => {
    const client = clientRef.current;
    clientRef.current = null;
    client?.close();
    signOut();
    setSession(null);
  }, []);

  if (!session) return <Login onSubmit={start} error={loginError} />;
  return <Connected session={session} onSignOut={logout} />;
}

function describe(code: string, message: string, welcomed: boolean): string {
  switch (code) {
    case "auth_failed":
      return "The server refused that key or token. Invite keys expire; ask an admin for a new one.";
    case "conflict":
      // Before any welcome, a conflict is the enrollment itself (e.g. an invite
      // that was already redeemed); after one, another session took over.
      return welcomed
        ? "This operator identity was taken over by another session."
        : `The server refused this sign-in: ${message}.`;
    default:
      return message;
  }
}
