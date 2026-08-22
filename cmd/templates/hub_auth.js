(() => {
  'use strict';
  const body = document.body;
  const mode = body.dataset.mode;
  const base = body.dataset.basePath || '';
  const endpoint = (path) => `${base}${path.startsWith('/') ? path : `/${path}`}`;
  const status = document.getElementById('authStatus');
  const form = document.getElementById('authForm');
  const button = document.getElementById('authButton');
  const b64urlToBytes = (value) => {
    const padded = String(value).replace(/-/g, '+').replace(/_/g, '/') + '==='.slice((String(value).length + 3) % 4);
    const raw = atob(padded); return Uint8Array.from(raw, c => c.charCodeAt(0));
  };
  const bytesToB64url = (value) => {
    const bytes = new Uint8Array(value); let raw = ''; for (const b of bytes) raw += String.fromCharCode(b);
    return btoa(raw).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/g, '');
  };
  const decodeCreation = (data) => { const p = data.publicKey; p.challenge=b64urlToBytes(p.challenge);p.user.id=b64urlToBytes(p.user.id);(p.excludeCredentials||[]).forEach(c=>c.id=b64urlToBytes(c.id));return data; };
  const decodeRequest = (data) => { const p=data.publicKey;p.challenge=b64urlToBytes(p.challenge);(p.allowCredentials||[]).forEach(c=>c.id=b64urlToBytes(c.id));return data; };
  const serialize = (credential) => ({id:credential.id,rawId:bytesToB64url(credential.rawId),type:credential.type,response:{clientDataJSON:bytesToB64url(credential.response.clientDataJSON),...(credential.response.attestationObject?{attestationObject:bytesToB64url(credential.response.attestationObject),transports:credential.response.getTransports?.()||[]}:{authenticatorData:bytesToB64url(credential.response.authenticatorData),signature:bytesToB64url(credential.response.signature),userHandle:credential.response.userHandle?bytesToB64url(credential.response.userHandle):null})},clientExtensionResults:credential.getClientExtensionResults?.()||{},authenticatorAttachment:credential.authenticatorAttachment||null});
  let grantVerified = sessionStorage.getItem('term_llm_hub_grant_verified') === '1';
  const post = async (path, value={}) => { const response=await fetch(endpoint(path),{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(value)});const data=await response.json().catch(()=>({}));if(!response.ok){const error=new Error(data?.error?.message||'Authentication failed');error.status=response.status;throw error;}return data; };
  const setGrantVerified = (verified) => { grantVerified=verified;if(verified)sessionStorage.setItem('term_llm_hub_grant_verified','1');else sessionStorage.removeItem('term_llm_hub_grant_verified'); };
  const register = async (prefix) => { const display_name=document.getElementById('displayName')?.value||'Primary passkey';const options=decodeCreation(await post(`${prefix}/register/begin`,{display_name}));setGrantVerified(true);const credential=await navigator.credentials.create(options);return post(`${prefix}/register/finish`,serialize(credential)); };
  const registerWithGrant = async (prefix) => { if(grantVerified){try{return await register(prefix);}catch(error){if(error.status!==401)throw error;setGrantVerified(false);}}await post(`${prefix}/verify`,{code:document.getElementById('setupCode').value});setGrantVerified(true);return register(prefix); };
  const login = async () => { const requested=new URLSearchParams(window.location.search).get('return')||endpoint('/');const options=decodeRequest(await post('/api/auth/login/begin',{return_path:requested}));const credential=await navigator.credentials.get(options);return post('/api/auth/login/finish',serialize(credential)); };
  const passkeysAvailable = () => Boolean(window.PublicKeyCredential && navigator.credentials);
  const authErrorMessage = (error) => error?.name === 'NotAllowedError' ? 'The passkey prompt was cancelled or timed out.' : (error?.message || 'Authentication failed.');
  if (form) form.addEventListener('submit',async(event)=>{event.preventDefault();button.disabled=true;status.classList.remove('error');status.textContent='Waiting for your passkey…';try{if(!passkeysAvailable())throw new Error('Passkeys require a supported browser, HTTPS, and the configured hostname.');let result;if(mode==='login'){result=await login();}else{const prefix=mode==='setup'?'/api/auth/bootstrap':'/api/auth/recovery';result=await registerWithGrant(prefix);}if(mode!=='login')setGrantVerified(false);window.location.assign(result.redirect||endpoint('/'));}catch(error){status.classList.add('error');status.textContent=authErrorMessage(error);button.disabled=false;}});
  const reauth = async () => { const options=decodeRequest(await post('/api/auth/reauth/begin',{}));const credential=await navigator.credentials.get(options);return post('/api/auth/reauth/finish',serialize(credential)); };
  const addPasskey = async (display_name) => { const options=decodeCreation(await post('/api/auth/credentials/register/begin',{display_name}));const credential=await navigator.credentials.create(options);return post('/api/auth/credentials/register/finish',serialize(credential)); };
  window.HubPasskey={post,reauth,addPasskey,registerWithGrant,setGrantVerified,endpoint,b64urlToBytes,bytesToB64url,decodeCreation,decodeRequest,serialize,passkeysAvailable,authErrorMessage};
  if (typeof module !== 'undefined') module.exports=window.HubPasskey;
})();
