/// <reference types="vite/client" />

import 'react';

declare module 'react' {
  interface HTMLAttributes<T> extends AriaAttributes, DOMAttributes<T> {
    toolname?: string;
    tooldescription?: string;
    toolparam?: string;
    toolparamdescription?: string;
  }
}
