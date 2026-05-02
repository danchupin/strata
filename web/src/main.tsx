import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { BrowserRouter } from 'react-router-dom';

import { App } from './App';
import { AuthProvider } from '@/components/auth-provider';
import { ThemeProvider } from '@/components/theme-provider';
import './styles/globals.css';

const container = document.getElementById('root');
if (!container) {
  throw new Error('root container missing in index.html');
}

createRoot(container).render(
  <StrictMode>
    <ThemeProvider>
      <BrowserRouter basename="/console">
        <AuthProvider>
          <App />
        </AuthProvider>
      </BrowserRouter>
    </ThemeProvider>
  </StrictMode>,
);
