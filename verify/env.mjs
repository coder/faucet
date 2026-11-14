// A prefix such as GO* also matches GOOGLE_APPLICATION_CREDENTIALS, so the Go variables are named one by one.
export const GO_ENV = ['GOROOT', 'GOPATH', 'GOCACHE', 'GOMODCACHE', 'GOFLAGS', 'GOTOOLCHAIN'];

const SYSTEM_ENV = ['PATH', 'HOME', 'TMPDIR', 'TMP', 'TEMP', 'SystemRoot', 'COMSPEC'];

export function childEnv(source = process.env) {
  const allowed = new Set([...SYSTEM_ENV, ...GO_ENV]);
  return Object.fromEntries(Object.entries(source).filter(([name, value]) => allowed.has(name) && value !== undefined));
}
