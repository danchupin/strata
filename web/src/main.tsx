import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';

import { App } from './App';
import { ThemeProvider } from '@/components/theme-provider';
import './styles/globals.css';

const container = document.getElementById('root');
if (!container) {
  throw new Error('root container missing in index.html');
}

createRoot(container).render(
  <StrictMode>
    <ThemeProvider>
      <App />
    </ThemeProvider>
  </StrictMode>,
);
