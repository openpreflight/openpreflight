The dialog component itself is not used; its script is.

`dialog.js` owns the `data-tui-dialog-*` markup contract that sheet/ and
alertdialog/ emit, and the JS bundle is every `components/*/*.js` concatenated,
so this file has to stay here for the sheet and the mobile sidebar to open.
Re-add the component with `shadcn-templ add dialog` if a page ever needs it.
