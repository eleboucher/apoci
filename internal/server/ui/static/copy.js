document.addEventListener('click', function(e) {
    if (!e.target.classList.contains('copy-btn')) return;
    var text = e.target.getAttribute('data-copy');
    if (!text) return;
    navigator.clipboard.writeText(text).then(function() {
        var original = e.target.textContent;
        e.target.textContent = 'Copied!';
        e.target.classList.add('copied');
        setTimeout(function() {
            e.target.textContent = original;
            e.target.classList.remove('copied');
        }, 1500);
    });
});
