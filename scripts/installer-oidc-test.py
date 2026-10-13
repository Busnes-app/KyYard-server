#!/usr/bin/env python3
"""Disposable real-binary installer and certificate-verified OIDC regression."""
import base64,hashlib,http.server,json,os,pathlib,socket,ssl,subprocess,tempfile,threading,time,urllib.error,urllib.parse,urllib.request
binary=os.environ.get('KY_INSTALLER_TEST_BINARY','')
if not os.path.isabs(binary) or not os.path.isfile(binary):
    raise SystemExit('Set KY_INSTALLER_TEST_BINARY to an absolute path to a built KyYard server binary')
def b64(raw):return base64.urlsafe_b64encode(raw).decode().rstrip('=')
class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self,*args):return None
opener=urllib.request.build_opener(NoRedirect())
with tempfile.TemporaryDirectory(prefix='kyq-yard-oidc-disposable-') as scratch:
    root=pathlib.Path(scratch);root.chmod(0o700)
    key,cert=root/'idp.key',root/'idp.crt'
    subprocess.run(['openssl','req','-x509','-newkey','rsa:2048','-nodes','-keyout',str(key),'-out',str(cert),'-days','1','-subj','/CN=localhost','-addext','subjectAltName=IP:127.0.0.1'],check=True,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
    modulus=subprocess.check_output(['openssl','rsa','-in',str(key),'-modulus','-noout'],stderr=subprocess.DEVNULL).decode().strip().split('=')[1]
    jwks={'keys':[{'kty':'RSA','kid':'fixture','use':'sig','alg':'RS256','n':b64(bytes.fromhex(modulus)),'e':'AQAB'}]}
    state={'sub':'owner','roles':['kyyard.admin'],'nonce':'','challenge':'','issuer':''}
    class IdP(http.server.BaseHTTPRequestHandler):
        def log_message(self,*args):pass
        def send(self,data,status=200):
            raw=json.dumps(data).encode();self.send_response(status);self.send_header('Content-Type','application/json');self.send_header('Content-Length',str(len(raw)));self.end_headers();self.wfile.write(raw)
        def do_GET(self):
            if self.path=='/.well-known/openid-configuration':self.send({'issuer':state['issuer'],'authorization_endpoint':state['issuer']+'/authorize','token_endpoint':state['issuer']+'/token','jwks_uri':state['issuer']+'/keys','response_types_supported':['code'],'subject_types_supported':['public'],'id_token_signing_alg_values_supported':['RS256']})
            elif self.path=='/keys':self.send(jwks)
            else:self.send({},404)
        def do_POST(self):
            form=urllib.parse.parse_qs(self.rfile.read(int(self.headers['Content-Length'])).decode())
            assert self.path=='/token' and form['code'][0]=='fixture-code'
            assert b64(hashlib.sha256(form['code_verifier'][0].encode()).digest())==state['challenge']
            assert form['redirect_uri'][0]=='https://yard.example/api/sso/kyidentity/callback'
            assert self.headers['Authorization']=='Basic '+base64.b64encode(b'yard:disposable-fixture-private-secret').decode()
            body={'iss':state['issuer'],'aud':'yard','sub':state['sub'],'nonce':state['nonce'],'preferred_username':'untrusted-display-name','role':'admin','app_roles':state['roles'],'iat':int(time.time()),'exp':int(time.time())+60}
            text=b64(json.dumps({'alg':'RS256','kid':'fixture','typ':'JWT'}).encode())+'.'+b64(json.dumps(body).encode())
            sig=subprocess.check_output(['openssl','dgst','-sha256','-sign',str(key)],input=text.encode())
            self.send({'access_token':'fixture-access','token_type':'Bearer','id_token':text+'.'+b64(sig)})
    idp=http.server.ThreadingHTTPServer(('127.0.0.1',0),IdP)
    context=ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER);context.load_cert_chain(str(cert),str(key));idp.socket=context.wrap_socket(idp.socket,server_side=True)
    state['issuer']=f'https://127.0.0.1:{idp.server_port}'
    idp_thread=threading.Thread(target=idp.serve_forever,daemon=True);idp_thread.start()
    with socket.socket() as sock:sock.bind(('127.0.0.1',0));port=sock.getsockname()[1]
    data=root/'data';data.mkdir(mode=0o700);private=data/'installer';private.mkdir(mode=0o700)
    for name in ('client.secret','directory.secret'):
        file=private/name;file.write_text('disposable-fixture-private-secret');file.chmod(0o600)
    env={'PATH':os.environ['PATH'],'SSL_CERT_FILE':str(cert),'KY_DATA_DIR':str(data),'KY_HOST':'127.0.0.1','KY_PORT':str(port),'KY_APP_URL':'https://yard.example','KY_TRUSTED_PROXIES':'127.0.0.1/32','KY_DOCKER_SOCKET':'','KY_ADMIN_PASSWORD':'disposable-fixture-password-only'}
    bundle={'schema':'ky.product-setup/1','service':'kyyard','setupId':'271333e2-c221-4601-b249-563b3d828530','sso':{'issuerUrl':state['issuer'],'clientId':'yard','clientSecretFile':str(private/'client.secret'),'directorySecretFile':str(private/'directory.secret'),'redirectUri':'https://yard.example/api/sso/kyidentity/callback'},'admins':[{'issuer':state['issuer'],'subject':'owner','username':'owner-admin'}],'everyday':[{'issuer':state['issuer'],'subject':'daily','username':'daily-user'}]}
    file=private/'setup.json';file.write_text(json.dumps(bundle));file.chmod(0o600)
    def request(path,cookies=None):
        req=urllib.request.Request(f'http://127.0.0.1:{port}'+path)
        if cookies:req.add_header('Cookie','; '.join(c.split(';',1)[0] for c in cookies))
        try:response=opener.open(req,timeout=15)
        except urllib.error.HTTPError as error:response=error
        body=response.read();return response.status,response.headers,body
    with (root/'runtime.log').open('w') as log:
        process=subprocess.Popen([binary],env=env,stdout=log,stderr=log)
        try:
            for _ in range(200):
                assert process.poll() is None,'server exited'
                if (data/'.installer/setup.sock').exists():
                    try:
                        if request('/api/settings')[0]==200:break
                    except OSError:pass
                time.sleep(.05)
            else:raise AssertionError('server unavailable')
            result=subprocess.run([binary,'apply-setup','--file',str(file)],env=env,capture_output=True,text=True,timeout=30)
            assert result.returncode==0,result.stderr
            assert len(json.loads(result.stdout)['created'])==3
            issued={}
            def login(sub,roles,want):
                state['sub']=sub;state['roles']=roles
                status,headers,_=request('/api/sso/kyidentity/login');assert status==302,status
                query=urllib.parse.parse_qs(urllib.parse.urlparse(headers['Location']).query)
                assert query['client_id']==['yard'] and query['code_challenge_method']==['S256']
                state['nonce']=query['nonce'][0];state['challenge']=query['code_challenge'][0]
                status,headers,body=request('/api/sso/kyidentity/callback?'+urllib.parse.urlencode({'state':query['state'][0],'code':'fixture-code'}),headers.get_all('Set-Cookie'))
                assert status==want,(sub,status,body.decode())
                cookies=headers.get_all('Set-Cookie') or []
                if want==302:
                    session=[c for c in cookies if c.startswith('ky_session=')];assert session,'session absent';issued[sub]=session
                else:assert not any(c.startswith('ky_session=') for c in cookies),'denied callback issued session'
            login('owner',['kyyard.admin'],302)
            login('daily',[],302)
            login('stranger',['kyyard.admin'],403)
            assert request('/api/backup/status',issued['owner'])[0]==200
            assert request('/api/backup/status',issued['daily'])[0]==403
            assert json.loads(request('/api/auth/me',issued['owner'])[2])['user']['username']=='owner-admin'
            assert json.loads(request('/api/auth/me',issued['daily'])[2])['user']['role']=='user','global admin claim promoted everyday account'
            login('owner',[],403)
            assert request('/api/backup/status',issued['owner'])[0]==401,'role-loss session survived'
            login('owner',['kyyard.admin'],403)
            assert request('/api/backup/status',issued['daily'])[0]==403
            print('PASS: actual binary + certificate-verified TLS OIDC, PKCE, signed issuer/audience/nonce, owner/daily/unassigned admission, global role ignored, durable role loss and session purge')
        finally:
            process.terminate();process.wait(timeout=30);idp.shutdown();idp.server_close();idp_thread.join(timeout=5)
