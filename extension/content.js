// Intercept fetch requests to capture the access token
const originalFetch = window.fetch;
window.fetch = async function(...args) {
  const response = await originalFetch.apply(this, args);
  
  try {
    const url = args[0]?.url || args[0];
    if (url?.includes('thislife.com')) {
      const body = args[1]?.body;
      if (body) {
        const parsed = JSON.parse(body);
        const token = parsed.params?.[0];
        if (token && token.startsWith('eyJ') && token.length > 500) {
          chrome.storage.local.set({ shutterflyToken: token, tokenTime: Date.now() });
          console.log('[Shutterfly Token Grabber] Token captured!');
        }
      }
    }
  } catch (e) {}
  
  return response;
};

// Also intercept XMLHttpRequest
const originalXHROpen = XMLHttpRequest.prototype.open;
const originalXHRSend = XMLHttpRequest.prototype.send;

XMLHttpRequest.prototype.open = function(method, url, ...rest) {
  this._url = url;
  return originalXHROpen.apply(this, [method, url, ...rest]);
};

XMLHttpRequest.prototype.send = function(body) {
  try {
    if (this._url?.includes('thislife.com') && body) {
      const parsed = JSON.parse(body);
      const token = parsed.params?.[0];
      if (token && token.startsWith('eyJ') && token.length > 500) {
        chrome.storage.local.set({ shutterflyToken: token, tokenTime: Date.now() });
        console.log('[Shutterfly Token Grabber] Token captured!');
      }
    }
  } catch (e) {}
  
  return originalXHRSend.apply(this, [body]);
};

console.log('[Shutterfly Token Grabber] Monitoring for tokens...');
