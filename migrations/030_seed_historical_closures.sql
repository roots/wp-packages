-- +goose Up
INSERT INTO closure_events (vendor_name, vendor_slug, detected_at, plugin_slugs, plugin_count)
VALUES (
    'Liton Arefin',
    'liton-arefin',
    '2026-05-08T11:00:38Z',
    '["post-modified-date","rolemaster-suite","spotlight"]',
    3
);

INSERT INTO closure_events (vendor_name, vendor_slug, detected_at, plugin_slugs, plugin_count)
VALUES (
    'bPlugins',
    'bplugins',
    '2026-04-29T16:02:25Z',
    '["3d-viewer","advanced-scrollbar","b-slider","b-social-share","cards-layout","document-embedder-addons-for-elementor","easy-twitter-feeds","recent-products-block","streamcast","timeline-block-block","video-gallery-block"]',
    11
);

INSERT INTO closure_events (vendor_name, vendor_slug, detected_at, plugin_slugs, plugin_count)
VALUES (
    'Essential Plugin',
    'essential-plugin',
    '2026-04-11T20:53:31Z',
    '["wp-trending-post-slider-and-widget","wp-testimonial-with-widget","wp-team-showcase-and-slider","wp-slick-slider-and-image-carousel","wp-responsive-recent-post-slider","wp-logo-showcase-responsive-slider-slider","wp-featured-content-and-slider","wp-blog-and-widgets","woo-product-slider-and-carousel-with-category","timeline-and-history-slider","ticker-ultimate","styles-for-wp-pagenavi-addon","sp-news-and-widget","sliderspack-all-in-one-image-sliders","product-categories-designs-for-woocommerce","preloader-for-website","post-grid-and-filter-ultimate","post-category-image-with-grid-and-slider","portfolio-and-projects","popup-anything-on-click","maintenance-mode-with-timer","html5-videogallery-plus-player","hero-banner-ultimate","footer-mega-grid-columns","featured-post-creative","countdown-timer-ultimate","blog-designer-for-post-and-widget","audio-player-with-playlist-ultimate","album-and-image-gallery-plus-lightbox","accordion-and-accordion-slider","sp-faq","meta-slider-and-carousel-with-lightbox","essential-chat-support"]',
    33
);

INSERT INTO closure_events (vendor_name, vendor_slug, detected_at, plugin_slugs, plugin_count)
VALUES (
    'WPFactory',
    'wpfactory',
    '2026-04-27T13:03:45Z',
    '["add-to-cart-button-labels-for-woocommerce","admin-bar-addition-for-woocommerce","ajax-product-search-woocommerce","amount-left-free-shipping-woocommerce","awesome-shortcodes","back-button-widget","bulk-price-converter-for-woocommerce","color-or-image-variation-swatches-for-woocommerce","compare-products-for-woocommerce","conditional-payment-gateways-for-woocommerce","content-excel-importer","cost-of-goods-for-woocommerce","coupon-by-user-role-for-woocommerce","crm-erp-business-solution","custom-checkout-fields-for-woocommerce","custom-css","custom-emails-for-woocommerce","custom-woo-cart-button","download-plugins-dashboard","ean-for-woocommerce","emails-verification-for-woocommerce","eu-vat-for-woocommerce","export-woocommerce","external-products-currency-for-woocommerce","file-renaming-on-upload","global-shop-discount-for-woocommerce","guest-order-tracking-for-woocommerce","instant-checkout-for-chatgpt-openai-readiness-for-woocommerce","maximum-products-per-user-for-woocommerce","msrp-for-woocommerce","my-woocommerce-product-virtual-showroom","order-minimum-amount-for-woocommerce","order-status-for-woocommerce","order-status-rules-for-woocommerce","payment-gateways-by-currency-for-woocommerce","payment-gateways-by-customer-location-for-woocommerce","payment-gateways-by-shipping-for-woocommerce","payment-gateways-per-product-categories-for-woocommerce","pdf-invoicing-for-woocommerce","popup-notices-for-woocommerce","price-offerings-for-woocommerce","product-notes-for-woocommerce","product-quantity-for-woocommerce","product-tabs-for-woocommerce","product-xml-feeds-for-woocommerce","products-per-page-for-woocommerce","products-stock-manager-with-excel","related-categories-for-woocommerce","remove-old-slugspermalinks","remove-special-characters-from-permalinks","stock-snapshot-for-woocommerce","stock-triggers-for-woocommerce","store-migration-products-orders-import-export-with-excel","support-ticket-system-for-woocommerce","url-coupons-for-woocommerce-by-algoritmika","users-import-export-with-excel-for-wp","webd-woocommerce-advanced-reporting-statistics","webd-woocommerce-product-excel-importer-bulk-edit","wholesale-pricing-woocommerce","wholesale-products-dynamic-pricing-management-woocommerce","wish-list-for-woocommerce","woo-product-excel-importer","world-population-counter","wp-currency-exchange-rates","wpfactory-conditional-shipping-for-woocommerce"]',
    65
);

INSERT INTO closure_events (vendor_name, vendor_slug, detected_at, plugin_slugs, plugin_count)
VALUES (
    'Algoritmika',
    'algoritmika',
    '2026-04-27T13:03:45Z',
    '["cart-messages-for-woocommerce","core-checkout-fields-for-woocommerce","custom-cart-and-checkout-info-for-woocommerce","discussions-tab-for-woocommerce-products","email-recipients-for-woocommerce","info-blocks-for-woocommerce","marketplace-for-woocommerce","my-account-customizer-for-woocommerce","price-robot-for-woocommerce","recent-orders-widget-for-woocommerce","sale-flash-customizer-for-woocommerce","shipping-calculator-customizer-for-woocommerce","zili-breadcrumbs-customizer-for-woocommerce","zili-coupon-code-generator-for-woocommerce","zili-user-products-for-woocommerce"]',
    15
);

INSERT INTO closure_events (vendor_name, vendor_slug, detected_at, plugin_slugs, plugin_count)
VALUES (
    'WBW Plugins',
    'wbw-plugins',
    '2026-04-27T13:03:45Z',
    '["woo-currency","woo-product-filter","woo-product-tables"]',
    3
);

-- +goose Down
DELETE FROM closure_events WHERE vendor_slug IN ('liton-arefin', 'bplugins', 'essential-plugin', 'wpfactory', 'algoritmika', 'wbw-plugins');
