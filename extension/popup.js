async function getToken() {
  const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
  
  if (!tab.url?.includes('shutterfly.com')) {
    return { error: 'Please navigate to shutterfly.com first' };
  }

  // Check storage for captured token
  const data = await chrome.storage.local.get(['shutterflyToken', 'tokenTime']);
  
  if (data.shutterflyToken) {
    // Check if token was captured recently (within 24 hours)
    const age = Date.now() - (data.tokenTime || 0);
    if (age < 24 * 60 * 60 * 1000) {
      return { token: data.shutterflyToken };
    }
  }
  
  return { error: 'Token not found. Click on an album or photo, then try again.' };
}

document.addEventListener('DOMContentLoaded', async () => {
  const status = document.getElementById('status');
  const tokenArea = document.getElementById('token');
  const copyBtn = document.getElementById('copy');

  const result = await getToken();

  if (result.token) {
    status.textContent = '✓ Token found!';
    status.className = 'success';
    tokenArea.value = result.token;
    copyBtn.disabled = false;
  } else {
    status.textContent = result.error;
    status.className = 'error';
  }

  copyBtn.addEventListener('click', async () => {
    await navigator.clipboard.writeText(tokenArea.value);
    copyBtn.textContent = 'Copied!';
    setTimeout(() => { copyBtn.textContent = 'Copy Token'; }, 2000);
  });
});
