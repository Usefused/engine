# Select

Use `Select` for dropdown fields throughout the Fused application. It renders a native
`select`, so keyboard navigation, mobile pickers, validation, form submission, refs,
and accessibility attributes continue to work normally.

```tsx
import { Select } from "~/components/forms/Select";

<Select
  aria-label="Filter by status"
  density="compact"
  tone="subtle"
  className="w-full sm:w-48"
  value={status}
  onChange={handleStatusChange}
>
  <option value="">All statuses</option>
  <option value="active">Active</option>
</Select>
```

- `density`: `regular` (default) or `compact` for filters and pagination.
- `tone`: `surface` (default) or `subtle`.
- `className` and `style`: page-specific width, layout, and presentation.
- Native props: `value`, `defaultValue`, `disabled`, `required`, `multiple`, `size`,
  `name`, event handlers, accessibility attributes, and tracking attributes.
- Children: native `option` and `optgroup` elements keep each page's choices local.

The shared stylesheet owns caret placement and reserved text space. Do not add a
second caret or page-specific arrow padding. Multiple selections and listboxes keep
their native presentation; forced-color mode restores the native dropdown indicator.
The select contract test prevents new raw dropdowns outside this component.

# Field labels

Use `FieldLabel` inside a semantic `label` for form fields across Fused. It shows
our neutral **Required** badge only when the user must provide a value. Keep
optional fields plain; filters and read-only summaries do not need a badge.

```tsx
import { FieldLabel } from "~/components/forms/FieldLabel";

<label htmlFor="app-name" className="block text-sm font-medium">
  <FieldLabel required>App name</FieldLabel>
</label>
<input id="app-name" name="name" required />
```

Pass the same boolean to the label and control for conditional requirements:
`<FieldLabel required={needsVersion}>Version</FieldLabel>` and
`<input required={needsVersion} />`. Keep existing validation and error messages;
the badge is a label, not an error. Associate labels with controls through
`htmlFor`/`id` or by nesting the control in the label. Custom controls must expose
the requirement accessibly and keep their existing submit validation. The badge
itself is hidden from assistive technology to avoid repeating native required state.

The component also works with `createElement(FieldLabel, { required: true }, "Name")`.
