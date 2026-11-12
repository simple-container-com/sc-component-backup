import { useEffect, useRef, useState, type FormEvent } from "react";
import { Navigate, useNavigate, useSearchParams } from "react-router-dom";
import { KeyRound, Terminal } from "lucide-react";
import { ApiError } from "@/api/client";
import { useLogin, useSession } from "@/api/queries";
import { Logo } from "@/components/layout/AppShell";
import { Button } from "@/components/ui/Button";
import { CopyButton } from "@/components/ui/CopyButton";
import { TextInput } from "@/components/ui/Field";
import { useDocumentTitle } from "@/lib/hooks";

const CLI = "docker compose run --rm db-backup set-ui-password";

export function LoginPage() {
  useDocumentTitle("Sign in");
  const session = useSession();
  const login = useLogin();
  const nav = useNavigate();
  const [params] = useSearchParams();
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const inputRef = useRef<HTMLInputElement>(null);
  const next = params.get("next");
  const target = next && next.startsWith("/") && !next.startsWith("//") ? next : "/";

  useEffect(() => inputRef.current?.focus(), [session.data?.password_set]);

  if (session.data?.authenticated) return <Navigate to={target} replace />;

  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (!password) {
      setError("Enter the password");
      return;
    }
    setError(null);
    login.mutate(password, {
      onSuccess: () => {
        setPassword("");
        nav(target, { replace: true });
      },
      onError: (err) => {
        setPassword("");
        if (err instanceof ApiError && err.status === 401) setError("Wrong password");
        else if (err instanceof ApiError && err.status === 429) setError("Too many attempts. Wait a minute.");
        else if (err instanceof ApiError && err.status === 0) setError("The server is not responding");
        else setError("Could not sign in");
        inputRef.current?.focus();
      },
    });
  };

  const noPassword = session.data && !session.data.password_set;

  return (
    <div className="flex min-h-screen items-start justify-center bg-bg px-4 pt-[14vh]">
      <div className="w-full max-w-[360px]">
        <div className="mb-8 flex items-center gap-2.5">
          <Logo className="h-8 w-8" />
          <div>
            <div className="text-lg font-semibold leading-tight tracking-tight">db-backup</div>
            <div className="text-xs text-muted">Database backups</div>
          </div>
        </div>

        {noPassword ? (
          <div className="card p-5">
            <div className="mb-2 flex items-center gap-2 font-medium">
              <KeyRound className="h-4 w-4 text-warn" aria-hidden />
              No password set yet
            </div>
            <p className="text-sm text-muted">
              Sign-in is disabled until the server has a password. Set it on the NAS in the service directory:
            </p>
            <div className="mt-3 flex items-center gap-1 rounded border border-line bg-sunken py-1 pl-3 pr-1">
              <Terminal className="h-3.5 w-3.5 shrink-0 text-faint" aria-hidden />
              <code className="min-w-0 flex-1 overflow-x-auto whitespace-nowrap py-1 text-xs">{CLI}</code>
              <CopyButton text={CLI} label="Copy command" />
            </div>
            <p className="mt-3 text-xs text-muted">Then reload the page.</p>
            <Button className="mt-4 w-full justify-center" onClick={() => void session.refetch()} loading={session.isFetching}>
              Check again
            </Button>
          </div>
        ) : (
          <form onSubmit={submit} className="card p-5" noValidate>
            <label htmlFor="password" className="mb-1.5 block text-sm font-medium">Admin password</label>
            <TextInput
              ref={inputRef}
              id="password"
              type="password"
              autoComplete="current-password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              invalid={!!error}
              disabled={session.isLoading}
            />
            {error && <p id="password-err" role="alert" className="mt-1.5 text-xs text-danger">{error}</p>}
            <Button type="submit" variant="primary" className="mt-4 w-full justify-center" loading={login.isPending}>
              Sign in
            </Button>
          </form>
        )}
      </div>
    </div>
  );
}
