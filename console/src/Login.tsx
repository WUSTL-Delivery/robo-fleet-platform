import { useState, type FormEvent } from "react";
import type { SignIn } from "./session";

type Mode = "invite" | "token";

export function Login({ onSubmit, error }: { onSubmit: (s: SignIn) => void; error?: string | undefined }) {
  const [mode, setMode] = useState<Mode>("invite");
  const [inviteKey, setInviteKey] = useState("");
  const [name, setName] = useState("");
  const [token, setToken] = useState("");

  const value = mode === "invite" ? inviteKey.trim() : token.trim();

  function submit(e: FormEvent) {
    e.preventDefault();
    if (!value) return;
    onSubmit(mode === "invite" ? { kind: "invite", inviteKey: value, name: name.trim() } : { kind: "token", token: value });
  }

  return (
    <div className="center">
      <form className="card" onSubmit={submit} aria-label="Sign in">
        <div className="brand">
          <img src="/favicon.svg" alt="" />
          <h1>Fleet console</h1>
        </div>
        <div className="tabs" role="tablist">
          <button type="button" role="tab" aria-selected={mode === "invite"} onClick={() => setMode("invite")}>
            Invite key
          </button>
          <button type="button" role="tab" aria-selected={mode === "token"} onClick={() => setMode("token")}>
            Operator token
          </button>
        </div>

        {mode === "invite" ? (
          <>
            <label htmlFor="invite">Invite key</label>
            <input
              id="invite"
              className="mono"
              autoComplete="off"
              spellCheck={false}
              placeholder="fp-oi-..."
              value={inviteKey}
              onChange={(e) => setInviteKey(e.target.value)}
              autoFocus
            />
            <label htmlFor="name">Your name</label>
            <input id="name" placeholder="alice" value={name} onChange={(e) => setName(e.target.value)} />
            <p className="hint">
              An invite works once. Redeeming it gives this browser an operator token, kept here so you stay
              signed in.
            </p>
          </>
        ) : (
          <>
            <label htmlFor="token">Operator token</label>
            <input
              id="token"
              className="mono"
              type="password"
              autoComplete="off"
              spellCheck={false}
              placeholder="fp-tk-..."
              value={token}
              onChange={(e) => setToken(e.target.value)}
              autoFocus
            />
            <p className="hint">A token you already redeemed, e.g. on another browser.</p>
          </>
        )}

        <button className="primary" type="submit" disabled={!value}>
          {mode === "invite" ? "Redeem invite" : "Sign in"}
        </button>
        {error && (
          <div className="error" role="alert">
            {error}
          </div>
        )}
      </form>
    </div>
  );
}
