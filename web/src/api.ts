function cookieValue(name: string): string {
  const prefix = `${encodeURIComponent(name)}=`;
  const item = document.cookie.split('; ').find((part) => part.startsWith(prefix));
  return item ? decodeURIComponent(item.slice(prefix.length)) : '';
}

export function secureFetch(input: RequestInfo | URL, init: RequestInit = {}): Promise<Response> {
  const headers = new Headers(init.headers);
  const csrf = cookieValue('ky_csrf');
  if (csrf) headers.set('X-CSRF-Token', csrf);
  return fetch(input, { ...init, headers });
}
