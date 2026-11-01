import { createRoot } from "react-dom/client";
import "@radix-ui/themes/styles.css";
import "golden-layout/dist/css/goldenlayout-base.css";
import "golden-layout/dist/css/themes/goldenlayout-dark-theme.css";
import "@xterm/xterm/css/xterm.css";
import App from "./App";

createRoot(document.getElementById("react-root")!).render(<App />);
