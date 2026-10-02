const form = document.querySelector('#post-form');
if (form) {
  const body = form.querySelector('#markdown');
  const title = form.querySelector('#title');
  const summary = form.querySelector('#summary');
  const previewBody = document.querySelector('#preview-body');
  const previewTitle = document.querySelector('#preview-title');
  const previewSummary = document.querySelector('#preview-summary');
  const tags = form.querySelector('#tags');
  const previewTags = document.querySelector('#preview-tags');
  let timer;
  let controller;
  function updatePreview() {
    previewTitle.textContent = title.value || 'Your title';
    previewSummary.textContent = summary.value || 'Your summary appears here.';
    previewTags.textContent = tags.value;
    clearTimeout(timer);
    timer = setTimeout(async () => {
      if (controller) controller.abort();
      controller = new AbortController();
      const data = new URLSearchParams({csrf: form.elements.csrf.value, markdown: body.value});
      try {
        const response = await fetch('/admin/preview', {method: 'POST', body: data, signal: controller.signal, credentials: 'same-origin'});
        if (response.ok) previewBody.innerHTML = await response.text();
      } catch (error) { if (error.name !== 'AbortError') previewBody.textContent = 'Preview is temporarily unavailable.'; }
    }, 250);
  }
  for (const field of [body, title, summary, tags]) field.addEventListener('input', updatePreview);
}
const deleteForm = document.querySelector('#delete-post-form');
if (deleteForm) {
  deleteForm.addEventListener('submit', event => {
    if (!window.confirm('Delete this post permanently?')) event.preventDefault();
  });
}
if (form) {
  const imageInput = form.querySelector('#image');
  const removeInput = form.querySelector('#remove-image');
  const previewImage = document.querySelector('#preview-image');
  const savedImage = previewImage?.getAttribute('src') || '';
  let objectURL = '';
  function updateImagePreview() {
    if (objectURL) URL.revokeObjectURL(objectURL);
    objectURL = '';
    if (imageInput.files.length) {
      objectURL = URL.createObjectURL(imageInput.files[0]);
      previewImage.src = objectURL;
      previewImage.hidden = false;
    } else if (removeInput?.checked || !savedImage) {
      previewImage.hidden = true;
      previewImage.removeAttribute('src');
    } else {
      previewImage.src = savedImage;
      previewImage.hidden = false;
    }
  }
  imageInput.addEventListener('change', updateImagePreview);
  removeInput?.addEventListener('change', updateImagePreview);
}
