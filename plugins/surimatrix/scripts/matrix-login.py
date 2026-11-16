#!/usr/bin/env python3
"""Create a fresh Matrix device; credentials are entered locally, never committed."""
import getpass
import json
import urllib.error
import urllib.parse
import urllib.request

server = input('Matrix homeserver (https://matrix.example.com): ').strip().rstrip('/')
parsed = urllib.parse.urlsplit(server)
if parsed.scheme != 'https' or not parsed.netloc or parsed.username or parsed.password:
    raise SystemExit('An HTTPS URL without embedded credentials is required.')
user = input('Bot account (@bot:example.com): ').strip()
password = getpass.getpass('Password (not stored): ')
body = json.dumps({'type': 'm.login.password', 'identifier': {
    'type': 'm.id.user', 'user': user}, 'password': password,
    'initial_device_display_name': 'Surimatrix 0.0.1'}).encode()
class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None
request = urllib.request.Request(server + '/_matrix/client/v3/login', data=body,
                                 headers={'Content-Type': 'application/json'})
try:
    with urllib.request.build_opener(NoRedirect).open(request, timeout=30) as response:
        result = json.load(response)
except urllib.error.HTTPError as exc:
    raise SystemExit(f'Matrix rejected the login: HTTP {exc.code}') from None
print('\nCopy these values only into the private Silo plugin settings:')
for key in ('user_id', 'device_id', 'access_token'):
    print(f'{key}: {result[key]}')
print('Do not log out this session from Element or reuse the device in another client.')
