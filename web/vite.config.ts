import type { UserConfig } from 'vite'
import { fileURLToPath } from 'node:url'
import react, { reactCompilerPreset } from '@vitejs/plugin-react';
import babel from '@rolldown/plugin-babel';
import tailwindcss from '@tailwindcss/vite'

// `persona server --web web/dist` serves the build. For `vite dev`, set
// VITE_PERSONA_SERVER to the server's address and run the server with
// --allow-origin <this dev server's origin>.
export default {
  plugins: [
    react(),
    babel({
      presets: [reactCompilerPreset()]
    }),
    tailwindcss()
  ],
  resolve: {
    alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) },
  },
} satisfies UserConfig
